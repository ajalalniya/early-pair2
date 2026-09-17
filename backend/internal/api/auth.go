package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/golang-jwt/jwt/v5"

	"github.com/earlypair/backend/internal/config"
	"github.com/earlypair/backend/internal/store"
)

// Auth issues sessions by verifying that the caller controls an address.
//
// This is the only place identity enters the service, and it establishes
// exactly one fact: the caller holds the key for an address. No key material is
// received, stored or transmitted — the client signs a nonce locally and sends
// the signature, which is all a signature is good for here.
type Auth struct {
	cfg   config.Auth
	store *store.Store
	log   *slog.Logger
}

func NewAuth(cfg config.Auth, st *store.Store, log *slog.Logger) *Auth {
	return &Auth{cfg: cfg, store: st, log: log.With("component", "auth")}
}

type ctxKey string

const ctxAddress ctxKey = "address"

// Require rejects a request without a valid session and puts the caller's
// address in the context.
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get("Authorization")
		if !strings.HasPrefix(raw, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "unauthenticated",
				"connect a wallet and sign in to use this endpoint")
			return
		}
		addr, err := a.verifyToken(strings.TrimPrefix(raw, "Bearer "))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid_session", "session is not valid")
			return
		}
		ctx := context.WithValue(r.Context(), ctxAddress, addr)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AddressFrom returns the authenticated address.
func AddressFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxAddress).(string)
	return v
}

func (a *Auth) secret() ([]byte, error) {
	if a.cfg.JWTSecret == "" {
		return nil, fmt.Errorf("auth: no JWT secret configured")
	}
	return []byte(a.cfg.JWTSecret), nil
}

func (a *Auth) issue(address string) (string, time.Time, error) {
	secret, err := a.secret()
	if err != nil {
		return "", time.Time{}, err
	}
	ttl := a.cfg.SessionTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	exp := time.Now().Add(ttl)

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": strings.ToLower(address),
		"iat": time.Now().Unix(),
		"exp": exp.Unix(),
	})
	signed, err := token.SignedString(secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, exp, nil
}

func (a *Auth) verifyToken(raw string) (string, error) {
	secret, err := a.secret()
	if err != nil {
		return "", err
	}
	tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		// Pinning the algorithm is what stops an attacker swapping in "none"
		// or an asymmetric method and having the library verify happily.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method %v", t.Header["alg"])
		}
		return secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil || !tok.Valid {
		return "", fmt.Errorf("auth: invalid token")
	}

	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return "", fmt.Errorf("auth: invalid claims")
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", fmt.Errorf("auth: no subject")
	}
	return sub, nil
}

type nonceRequest struct {
	Address string `json:"address"`
}

func (s *Server) handleAuthNonce(w http.ResponseWriter, r *http.Request) {
	var req nonceRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !common.IsHexAddress(req.Address) {
		writeError(w, http.StatusBadRequest, "bad_address", "address is not a valid hex address")
		return
	}

	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not generate a nonce")
		return
	}
	nonce := hex.EncodeToString(buf)

	ttl := s.cfg.Auth.NonceTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	expires := time.Now().Add(ttl)

	if err := s.store.PutNonce(r.Context(), nonce, strings.ToLower(req.Address), expires); err != nil {
		writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"nonce":     nonce,
		"expiresAt": expires,
		"message":   siweMessage(s.cfg.Auth.Domain, req.Address, nonce, expires),
	})
}

type verifyRequest struct {
	Address   string `json:"address"`
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
}

func (s *Server) handleAuthVerify(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if !common.IsHexAddress(req.Address) {
		writeError(w, http.StatusBadRequest, "bad_address", "address is not a valid hex address")
		return
	}

	// Nonces are single use. Consuming it before verifying the signature means
	// a replayed signature cannot be retried against the same nonce.
	claimed, expires, err := s.store.ConsumeNonce(r.Context(), req.Nonce)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "bad_nonce", "nonce is unknown, used or expired")
		return
	}
	if !strings.EqualFold(claimed, req.Address) {
		writeError(w, http.StatusUnauthorized, "nonce_mismatch",
			"this nonce was issued for a different address")
		return
	}

	message := siweMessage(s.cfg.Auth.Domain, req.Address, req.Nonce, expires)
	recovered, err := recoverSigner(message, req.Signature)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "bad_signature", "signature could not be recovered")
		return
	}
	if !strings.EqualFold(recovered, req.Address) {
		writeError(w, http.StatusUnauthorized, "wrong_signer",
			"the signature does not belong to that address")
		return
	}

	token, exp, err := s.auth.issue(req.Address)
	if err != nil {
		s.log.Error("could not issue a session", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not issue a session")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"token":     token,
		"expiresAt": exp,
		"address":   strings.ToLower(req.Address),
	})
}

// siweMessage is the exact text the wallet signs. It must be reproduced
// byte-for-byte at verification time, so it is built in one place.
func siweMessage(domain, address, nonce string, expires time.Time) string {
	if domain == "" {
		domain = "earlypair"
	}
	return strings.Join([]string{
		domain + " wants you to sign in with your Ethereum account:",
		common.HexToAddress(address).Hex(),
		"",
		"Signing proves you control this address. It authorises nothing else: " +
			"EarlyPair never takes custody, and every trade is approved as its own transaction.",
		"",
		"Nonce: " + nonce,
		"Expires: " + expires.UTC().Format(time.RFC3339),
	}, "\n")
}

// recoverSigner recovers the address from a personal_sign signature.
func recoverSigner(message, signature string) (string, error) {
	sig := common.FromHex(signature)
	if len(sig) != 65 {
		return "", fmt.Errorf("auth: signature must be 65 bytes, got %d", len(sig))
	}
	// Wallets emit v as 27/28; the recovery code wants 0/1.
	switch sig[64] {
	case 27, 28:
		sig[64] -= 27
	case 0, 1:
	default:
		return "", fmt.Errorf("auth: unexpected signature recovery id %d", sig[64])
	}

	prefixed := accounts191Hash(message)
	pub, err := crypto.SigToPub(prefixed, sig)
	if err != nil {
		return "", err
	}
	return crypto.PubkeyToAddress(*pub).Hex(), nil
}

// accounts191Hash applies the EIP-191 personal-sign prefix.
func accounts191Hash(message string) []byte {
	prefixed := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(message), message)
	return crypto.Keccak256([]byte(prefixed))
}
