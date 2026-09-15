package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Suknna/quoin/internal/quoin/auth"
	"github.com/Suknna/quoin/internal/quoin/delivery"
	"github.com/Suknna/quoin/internal/quoin/secrets"
)

type authDeliveryChannel struct {
	Kind              string            `json:"kind"`
	Host              string            `json:"host,omitempty"`
	Port              int               `json:"port,omitempty"`
	From              string            `json:"from,omitempty"`
	Username          string            `json:"username,omitempty"`
	PasswordRef       string            `json:"passwordRef,omitempty"`
	TLSMode           string            `json:"tlsMode,omitempty"`
	URL               string            `json:"url,omitempty"`
	Headers           map[string]string `json:"headers,omitempty"`
	SecretHeaders     map[string]string `json:"secretHeaders,omitempty"`
	Encoding          string            `json:"encoding,omitempty"`
	Fields            map[string]string `json:"fields,omitempty"`
	SuccessField      string            `json:"successField,omitempty"`
	SuccessValue      string            `json:"successValue,omitempty"`
	AllowPrivateCIDRs []string          `json:"allowPrivateCIDRs,omitempty"`
	RootCAPEM         string            `json:"rootCaPem,omitempty"`
}

type authDeliveryConfiguration struct {
	Email *authDeliveryChannel `json:"email,omitempty"`
	SMS   *authDeliveryChannel `json:"sms,omitempty"`
}

type authDeliverySender struct{ application *apiServer }

func (s authDeliverySender) Send(ctx context.Context, msg auth.Message) error {
	config, secretValues, _, _, err := s.application.loadAuthDelivery(ctx)
	if err != nil {
		return err
	}
	channel := config.Email
	if msg.Channel == "sms" {
		channel = config.SMS
	}
	if channel == nil {
		return errors.New("authentication delivery channel is not configured")
	}
	sender, err := buildAuthSender(channel, secretValues)
	if err != nil {
		return errors.New("authentication delivery configuration is invalid")
	}
	seconds, err := strconv.Atoi(msg.Variables["expires_in_seconds"])
	if err != nil || seconds < 1 {
		return errors.New("authentication delivery expiry is invalid")
	}
	return sender.Send(ctx, delivery.Message{ID: msg.DeliveryID, Channel: delivery.Channel(msg.Channel), Recipient: msg.Recipient, Template: msg.Template, Code: msg.Variables["code"], ExpiresIn: time.Duration(seconds) * time.Second})
}

func buildAuthSender(config *authDeliveryChannel, secretValues map[string]string) (delivery.Sender, error) {
	if config == nil {
		return nil, errors.New("missing delivery configuration")
	}
	resolve := func(_ context.Context, ref string) (string, error) {
		value, ok := secretValues[ref]
		if !ok {
			return "", errors.New("delivery secret reference is unavailable")
		}
		return value, nil
	}
	switch config.Kind {
	case "smtp":
		return delivery.NewSMTPSender(delivery.SMTPConfig{Host: config.Host, Port: config.Port, From: config.From, Username: config.Username, PasswordRef: config.PasswordRef, TLSMode: config.TLSMode, AllowPrivateCIDRs: config.AllowPrivateCIDRs, RootCAPEM: []byte(config.RootCAPEM)}, resolve)
	case "webhook":
		return delivery.NewWebhookSender(delivery.WebhookConfig{URL: config.URL, Headers: config.Headers, SecretHeaders: config.SecretHeaders, Encoding: config.Encoding, JSONFields: config.Fields, FormFields: config.Fields, SuccessField: config.SuccessField, SuccessValue: config.SuccessValue, AllowPrivateCIDRs: config.AllowPrivateCIDRs, RootCAPEM: []byte(config.RootCAPEM)}, resolve)
	default:
		return nil, errors.New("unsupported delivery kind")
	}
}

func (application *apiServer) configureAuthentication() error {
	rootKey, err := application.rootKey()
	if err != nil {
		return err
	}
	if len(rootKey) != 32 {
		return errors.New("authentication requires a valid root key")
	}
	mac := hmac.New(sha256.New, rootKey)
	_, _ = mac.Write([]byte("quoin:authentication:otp:v1"))
	return application.auth.ConfigureAuth(auth.AuthConfig{OTPKey: mac.Sum(nil), Sender: authDeliverySender{application}})
}

func (application *apiServer) loadAuthDelivery(ctx context.Context) (authDeliveryConfiguration, map[string]string, int64, string, error) {
	var config authDeliveryConfiguration
	var source, encoded string
	var nonce, ciphertext []byte
	var revision int64
	var binding int
	err := application.db.QueryRowContext(ctx, `SELECT source,configuration_json,secret_nonce,secret_ciphertext,root_binding_revision,row_version FROM auth_delivery_settings WHERE id=1`).Scan(&source, &encoded, &nonce, &ciphertext, &binding, &revision)
	if err != nil {
		return config, nil, 0, "", err
	}
	if err := json.Unmarshal([]byte(encoded), &config); err != nil {
		return config, nil, 0, "", errors.New("invalid stored authentication delivery settings")
	}
	values := map[string]string{}
	if len(ciphertext) > 0 {
		key, err := application.rootKey()
		if err != nil {
			return config, nil, 0, "", err
		}
		plain, err := secrets.OpenSetting(key, "auth.delivery", revision, binding, &secrets.Envelope{Nonce: nonce, Ciphertext: ciphertext})
		if err != nil {
			return config, nil, 0, "", err
		}
		if err := json.Unmarshal(plain, &values); err != nil {
			return config, nil, 0, "", errors.New("invalid authentication delivery secrets")
		}
	}
	return config, values, revision, source, nil
}

func validateAuthDelivery(config authDeliveryConfiguration, values map[string]string) error {
	if config.Email == nil && config.SMS == nil {
		return errors.New("at least one delivery channel is required")
	}
	if config.SMS != nil && config.SMS.Kind != "webhook" {
		return errors.New("SMS requires webhook delivery")
	}
	for _, channel := range []*authDeliveryChannel{config.Email, config.SMS} {
		if channel == nil {
			continue
		}
		if _, err := buildAuthSender(channel, values); err != nil {
			return fmt.Errorf("invalid delivery configuration: %w", err)
		}
		for _, ref := range channel.SecretHeaders {
			if _, ok := values[ref]; !ok {
				return errors.New("missing secret reference")
			}
		}
		if channel.PasswordRef != "" {
			if _, ok := values[channel.PasswordRef]; !ok {
				return errors.New("missing SMTP secret reference")
			}
		}
	}
	return nil
}
