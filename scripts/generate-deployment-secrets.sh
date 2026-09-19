#!/usr/bin/env bash
# Deployment secret generation for operators (ADR-0009 mTLS identities).
#
# The product binary deliberately does not generate deployment secrets;
# first-boot material is a deployment responsibility. This script produces the
# complete fixed secret set the manifests expect:
#
#   root-key            32 raw random bytes (credential envelope root key)
#   runtime-ca.pem/key  Runtime CA (EC P-256, 10y)
#   runtime-tls.crt/key quoin:8443 server identity (CN=quoin, SAN quoin+localhost, 2y)
#   stele-client.crt/key   client identity CN=stele  (10y, clientAuth)
#   plinth-client.crt/key  client identity CN=plinth (10y, clientAuth)
#
# Usage:
#   scripts/generate-deployment-secrets.sh <secrets-dir>
#   scripts/generate-deployment-secrets.sh <secrets-dir> --issue-client-certs [--force]
#
# The first form refuses any pre-existing file in <secrets-dir>: reruns on a
# live deployment would invalidate existing identities. --issue-client-certs
# re-signs only the two client certificates from the existing CA (rotation or
# the single-component hotfix runbook); --force replaces existing client
# certificate files.
set -euo pipefail

if [[ $# -lt 1 ]]; then
	echo "usage: $0 <secrets-dir> [--issue-client-certs [--force]]" >&2
	exit 2
fi
directory=$1
shift || true
mode=bootstrap
force=0
for argument in "$@"; do
	case "$argument" in
	--issue-client-certs) mode=client-certs ;;
	--force) force=1 ;;
	*)
		echo "unknown argument: $argument" >&2
		exit 2
		;;
	esac
done

all_files=(root-key runtime-ca.pem runtime-ca.key runtime-tls.crt runtime-tls.key stele-client.crt stele-client.key plinth-client.crt plinth-client.key)

mkdir -p "$directory"
chmod 700 "$directory"
cd "$directory"

if [[ "$mode" == "bootstrap" ]]; then
	for name in "${all_files[@]}"; do
		if [[ -e "$name" ]]; then
			echo "refusing to touch existing secret: $directory/$name (reruns would invalidate live identities)" >&2
			exit 1
		fi
	done
	umask 077
	head -c 32 /dev/urandom >root-key

	openssl ecparam -name prime256v1 -genkey -noout -out runtime-ca.key
	openssl req -new -x509 -key runtime-ca.key -out runtime-ca.pem -days 3650 \
		-subj '/CN=Quoin Runtime CA' \
		-addext 'basicConstraints=critical,CA:TRUE' \
		-addext 'keyUsage=critical,keyCertSign,digitalSignature'

	openssl ecparam -name prime256v1 -genkey -noout -out runtime-tls.key
	openssl req -new -key runtime-tls.key -subj '/CN=quoin' -out runtime-tls.csr
	printf '%s\n' 'subjectAltName=DNS:quoin,DNS:localhost' \
		'extendedKeyUsage=serverAuth' 'basicConstraints=critical,CA:FALSE' \
		'keyUsage=critical,digitalSignature' >runtime-tls.ext
	openssl x509 -req -in runtime-tls.csr -CA runtime-ca.pem -CAkey runtime-ca.key \
		-CAcreateserial -out runtime-tls.crt -days 730 -extfile runtime-tls.ext
	rm -f runtime-tls.csr runtime-tls.ext runtime-ca.srl
else
	for name in runtime-ca.pem runtime-ca.key; do
		if [[ ! -f "$name" ]]; then
			echo "rotation requires the existing Runtime CA: $directory/$name missing" >&2
			exit 1
		fi
	done
	if [[ "$force" != 1 ]]; then
		for name in stele-client.crt stele-client.key plinth-client.crt plinth-client.key; do
			if [[ -e "$name" ]]; then
				echo "refusing to replace existing $name (pass --force)" >&2
				exit 1
			fi
		done
	fi
fi

if [[ "$mode" == "bootstrap" || "$mode" == "client-certs" ]]; then
	umask 077
	for component in stele plinth; do
		openssl ecparam -name prime256v1 -genkey -noout -out "$component-client.key"
		openssl req -new -key "$component-client.key" -subj "/CN=$component" -out "$component-client.csr"
		printf '%s\n' 'extendedKeyUsage=clientAuth' 'basicConstraints=critical,CA:FALSE' \
			'keyUsage=critical,digitalSignature' >"$component-client.ext"
		openssl x509 -req -in "$component-client.csr" -CA runtime-ca.pem -CAkey runtime-ca.key \
			-CAcreateserial -out "$component-client.crt" -days 3650 -extfile "$component-client.ext"
		rm -f "$component-client.csr" "$component-client.ext"
	done
	rm -f runtime-ca.srl
fi

chmod 600 ./* 2>/dev/null || true
chmod 600 root-key runtime-ca.key runtime-tls.key stele-client.key plinth-client.key 2>/dev/null || true

# Verify the set the same way the components will consume it.
openssl verify -CAfile runtime-ca.pem runtime-tls.crt stele-client.crt plinth-client.crt >/dev/null
openssl x509 -in stele-client.crt -noout -subject | grep -q 'CN=stele'
openssl x509 -in plinth-client.crt -noout -subject | grep -q 'CN=plinth'
[[ $(stat -c %s root-key) -eq 32 ]] || {
	echo 'root-key must be 32 raw bytes' >&2
	exit 1
}

echo "deployment secrets ready in $directory"
