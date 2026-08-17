package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/unicode/norm"
)

const (
	argonMemory  = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
)

//go:embed passwords/100k-most-used-passwords-NCSC.txt
var passwordAssets embed.FS

var (
	blocklistOnce sync.Once
	blocked       map[string]struct{}
	blocklistErr  error
)

type Passwords struct {
	dummyPHC string
}

func NewPasswords() (*Passwords, error) {
	dummy, err := HashPassword("quoin-dummy-password-never-accepted")
	if err != nil {
		return nil, err
	}
	return &Passwords{dummyPHC: dummy}, nil
}

func NormalizePassword(value string) (string, error) {
	normalized := norm.NFC.String(value)
	length := utf8.RuneCountInString(normalized)
	if length < 15 || length > 128 {
		return "", fmt.Errorf("password must contain 15 to 128 Unicode characters")
	}
	return normalized, nil
}

func NormalizeUsername(value string) string {
	return strings.ToLower(norm.NFC.String(strings.TrimSpace(value)))
}

func ValidateNewPassword(value, username, displayName string) (string, error) {
	normalized, err := NormalizePassword(value)
	if err != nil {
		return "", err
	}
	if err := loadBlocklist(); err != nil {
		return "", err
	}
	key := strings.ToLower(normalized)
	if _, exists := blocked[key]; exists {
		return "", fmt.Errorf("choose a password that is not commonly used")
	}
	for _, contextValue := range []string{"quoin", NormalizeUsername(username), strings.ToLower(norm.NFC.String(strings.TrimSpace(displayName)))} {
		if contextValue != "" && strings.EqualFold(normalized, contextValue) {
			return "", fmt.Errorf("password must not equal the product, username, or display name")
		}
	}
	return normalized, nil
}

func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifyPassword(password, phc string) bool {
	memory, iterations, threads, salt, expected, ok := parsePHC(phc)
	if !ok {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func (passwords *Passwords) VerifyDummy(password string) {
	_ = VerifyPassword(password, passwords.dummyPHC)
}

func parsePHC(phc string) (uint32, uint32, uint8, []byte, []byte, bool) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return 0, 0, 0, nil, nil, false
	}
	var memory, iterations uint64
	var threadsValue uint64
	for _, setting := range strings.Split(parts[3], ",") {
		name, value, found := strings.Cut(setting, "=")
		if !found {
			return 0, 0, 0, nil, nil, false
		}
		parsed, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return 0, 0, 0, nil, nil, false
		}
		switch name {
		case "m":
			memory = parsed
		case "t":
			iterations = parsed
		case "p":
			threadsValue = parsed
		default:
			return 0, 0, 0, nil, nil, false
		}
	}
	if memory < argonMemory || iterations < argonTime || threadsValue < argonThreads || threadsValue > 255 {
		return 0, 0, 0, nil, nil, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return 0, 0, 0, nil, nil, false
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(hash) < 16 {
		return 0, 0, 0, nil, nil, false
	}
	return uint32(memory), uint32(iterations), uint8(threadsValue), salt, hash, true
}

func loadBlocklist() error {
	blocklistOnce.Do(func() {
		data, err := passwordAssets.ReadFile("passwords/100k-most-used-passwords-NCSC.txt")
		if err != nil {
			blocklistErr = err
			return
		}
		blocked = make(map[string]struct{}, 99840)
		for _, line := range strings.Split(string(data), "\n") {
			if line != "" {
				blocked[strings.ToLower(norm.NFC.String(strings.TrimSpace(line)))] = struct{}{}
			}
		}
	})
	return blocklistErr
}
