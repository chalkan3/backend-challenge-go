//go:build integration

// Package integration runs against the REAL stack: PostgreSQL, Keycloak and
// LocalStack provisioned by docker-compose (challenge section 13 — no
// wholesale mocking of infrastructure).
//
// Required environment (defaults match the compose file):
//
//	WALLET_TEST_API_URL      http://localhost:8081
//	WALLET_TEST_API_URL_2    http://localhost:8082   (profile multi; optional)
//	WALLET_TEST_DB_DSN       postgres://wallet:wallet@localhost:5433/wallet?sslmode=disable
//	WALLET_TEST_SQS_ENDPOINT http://localhost:4566
//	WALLET_TEST_KEYCLOAK     http://localhost:8080
package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	realm           = "wallet"
	providerAClient = "provider-a"
	providerASecret = "provider-a-secret"
	providerBClient = "provider-b"
	providerBSecret = "provider-b-secret"
	internalClient  = "wallet-internal"
	internalSecret  = "internal-secret"
)

func apiURL(t *testing.T) string {
	t.Helper()
	v := envOr("WALLET_TEST_API_URL", "http://localhost:8081")
	return strings.TrimSuffix(v, "/")
}

func apiURL2(t *testing.T) string {
	t.Helper()
	return strings.TrimSuffix(envOr("WALLET_TEST_API_URL_2", "http://localhost:8082"), "/")
}

func dbDSN(t *testing.T) string {
	t.Helper()
	return envOr("WALLET_TEST_DB_DSN", "postgres://wallet:wallet@localhost:5433/wallet?sslmode=disable")
}

func sqsEndpoint(t *testing.T) string {
	t.Helper()
	return envOr("WALLET_TEST_SQS_ENDPOINT", "http://localhost:4566")
}

func keycloakURL(t *testing.T) string {
	t.Helper()
	return strings.TrimSuffix(envOr("WALLET_TEST_KEYCLOAK", "http://localhost:8080"), "/")
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

var testLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))

// mustDB connects a fresh pool (own connections per test → different
// "instances" at the connection level).
func mustDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbDSN(t))
	if err != nil {
		t.Fatalf("db connect: %v", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		t.Fatalf("db ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func mustSQS(t *testing.T) *sqs.Client {
	t.Helper()
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(sqsEndpoint(t)) })
}

func mustQueueURL(t *testing.T, client *sqs.Client, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		t.Fatalf("queue %s: %v", name, err)
	}
	return *out.QueueUrl
}

// token fetches a client_credentials token from Keycloak.
func token(t *testing.T, clientID, secret string) string {
	t.Helper()
	body := fmt.Sprintf("grant_type=client_credentials&client_id=%s&client_secret=%s", clientID, secret)
	req, _ := http.NewRequest("POST", keycloakURL(t)+"/realms/"+realm+"/protocol/openid-connect/token",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("token request: status %d body %s", resp.StatusCode, raw)
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.AccessToken == "" {
		t.Fatalf("token parse: %v %s", err, raw)
	}
	return parsed.AccessToken
}

// api performs an authenticated JSON call and returns status + parsed body.
func api(t *testing.T, method, path, bearer string, body any) (int, map[string]any) {
	t.Helper()
	return apiAt(t, apiURL(t), method, path, bearer, body)
}

func apiAt(t *testing.T, base, method, path, bearer string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, parsed
}

// submitTransaction POSTs an operation with an idempotency key.
func submitTransaction(t *testing.T, bearer string, body map[string]any, idemKey string) (int, map[string]any) {
	t.Helper()
	return apiWithHeader(t, apiURL(t), "POST", "/wagering/transactions", bearer, body, "Idempotency-Key", idemKey)
}

func apiWithHeader(t *testing.T, base, method, path, bearer string, body any, hKey, hVal string) (int, map[string]any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest(method, base+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(hKey, hVal)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, parsed
}

func newPlayerID(t *testing.T) string {
	t.Helper()
	return uuid.NewString()
}

// errCode extracts error.code from the transport's error envelope
// ({"error":{"code":...,"message":...}}), returning "" when absent.
func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// openWallet creates a wallet with the internal identity.
func openWallet(t *testing.T, internalTok, playerID, amount string) string {
	t.Helper()
	status, body := api(t, "POST", "/wallets", internalTok, map[string]any{
		"playerId":       playerID,
		"initialBalance": map[string]string{"amount": amount, "currency": "BRL"},
	})
	if status != http.StatusCreated {
		t.Fatalf("open wallet: status %d body %v", status, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("open wallet: no id in %v", body)
	}
	return id
}

func walletBalance(t *testing.T, bearer, walletID string) (string, int64) {
	t.Helper()
	status, body := api(t, "GET", "/wallets/"+walletID, bearer, nil)
	if status != 200 {
		t.Fatalf("get wallet: %d %v", status, body)
	}
	bal, _ := body["balance"].(map[string]any)
	amount, _ := bal["amount"].(string)
	version, _ := body["version"].(float64)
	return amount, int64(version)
}

// pollUntil retries fn until it returns true or the deadline passes.
func pollUntil(t *testing.T, d time.Duration, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", msg)
}

func sha256HexStr(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// clientDo performs a raw form-urlencoded request (used for negative IdP
// credential tests).
func clientDo(t *testing.T, method, url, formBody string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(formBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return http.DefaultClient.Do(req)
}
