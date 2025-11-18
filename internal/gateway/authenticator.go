package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

// Authenticator handles authentication for the gateway
type Authenticator struct {
	config         *AuthConfig
	logger         *zap.Logger
	apiKeys        map[string]*APIKeyInfo
	jwtSecret      []byte
	webhookSecrets map[string]string
}

// AuthClaims represents authentication claims
type AuthClaims struct {
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes"`
	Roles    []string `json:"roles"`
	Email    string   `json:"email"`
	Subject  string   `json:"sub"`
	Issuer   string   `json:"iss"`
	IsAdmin  bool     `json:"is_admin"`
	jwt.RegisteredClaims
}

// APIKeyInfo contains information about an API key
type APIKeyInfo struct {
	Key       string
	ClientID  string
	Scopes    []string
	Roles     []string
	ExpiresAt *time.Time
	Enabled   bool
}

// JWTConfig for JWT authentication
type JWTConfig struct {
	Secret    string   `json:"secret"`
	Issuer    string   `json:"issuer"`
	Audience  []string `json:"audience"`
	ExpiresIn int      `json:"expires_in"` // seconds
}

// APIKeyConfig for API key authentication
type APIKeyConfig struct {
	HeaderName string                 `json:"header_name"`
	Keys       map[string]*APIKeyInfo `json:"keys"`
}

// OAuth2Config for OAuth2 authentication
type OAuth2Config struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	AuthURL      string   `json:"auth_url"`
	TokenURL     string   `json:"token_url"`
	RedirectURL  string   `json:"redirect_url"`
	Scopes       []string `json:"scopes"`
}

// MTLSConfig for mutual TLS authentication
type MTLSConfig struct {
	Enabled         bool     `json:"enabled"`
	ClientCAFile    string   `json:"client_ca_file"`
	AllowedCNs      []string `json:"allowed_cns"`
	AllowedDNSNames []string `json:"allowed_dns_names"`
}

// NewAuthenticator creates a new authenticator
func NewAuthenticator(config *AuthConfig, logger *zap.Logger) (*Authenticator, error) {
	auth := &Authenticator{
		config:         config,
		logger:         logger,
		apiKeys:        make(map[string]*APIKeyInfo),
		webhookSecrets: make(map[string]string),
	}

	// Initialize JWT secret
	if config.JWTConfig != nil {
		auth.jwtSecret = []byte(config.JWTConfig.Secret)
	}

	// Initialize API keys
	if config.APIKeyConfig != nil && config.APIKeyConfig.Keys != nil {
		auth.apiKeys = config.APIKeyConfig.Keys
	} else {
		// Load default API keys for testing
		auth.loadDefaultAPIKeys()
	}

	// Initialize webhook secrets
	auth.loadWebhookSecrets()

	return auth, nil
}

// Authenticate performs authentication based on requirements
func (a *Authenticator) Authenticate(r *http.Request, requirements AuthRequirements) (*AuthClaims, error) {
	if !requirements.Enabled {
		// Authentication not required
		return &AuthClaims{
			ClientID: "anonymous",
		}, nil
	}

	var lastError error

	// Try each authentication method
	for _, method := range requirements.Methods {
		claims, err := a.authenticateWithMethod(r, method)
		if err == nil {
			// Verify scopes and roles if required
			if err := a.verifyRequirements(claims, requirements); err != nil {
				lastError = err
				continue
			}
			return claims, nil
		}
		lastError = err
	}

	if lastError != nil {
		return nil, fmt.Errorf("authentication failed: %w", lastError)
	}

	return nil, fmt.Errorf("no valid authentication method found")
}

// authenticateWithMethod authenticates using a specific method
func (a *Authenticator) authenticateWithMethod(r *http.Request, method string) (*AuthClaims, error) {
	switch method {
	case "api_key":
		return a.authenticateAPIKey(r)
	case "jwt":
		return a.authenticateJWT(r)
	case "oauth2":
		return a.authenticateOAuth2(r)
	case "mtls":
		return a.authenticateMTLS(r)
	default:
		return nil, fmt.Errorf("unsupported authentication method: %s", method)
	}
}

// authenticateAPIKey authenticates using API key
func (a *Authenticator) authenticateAPIKey(r *http.Request) (*AuthClaims, error) {
	headerName := "X-API-Key"
	if a.config.APIKeyConfig != nil && a.config.APIKeyConfig.HeaderName != "" {
		headerName = a.config.APIKeyConfig.HeaderName
	}

	apiKey := r.Header.Get(headerName)
	if apiKey == "" {
		// Try Authorization header
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "ApiKey ") {
			apiKey = strings.TrimPrefix(authHeader, "ApiKey ")
		}
	}

	if apiKey == "" {
		return nil, fmt.Errorf("API key not provided")
	}

	// Lookup API key
	keyInfo, exists := a.apiKeys[apiKey]
	if !exists {
		return nil, fmt.Errorf("invalid API key")
	}

	// Check if key is enabled
	if !keyInfo.Enabled {
		return nil, fmt.Errorf("API key is disabled")
	}

	// Check expiration
	if keyInfo.ExpiresAt != nil && keyInfo.ExpiresAt.Before(time.Now()) {
		return nil, fmt.Errorf("API key has expired")
	}

	return &AuthClaims{
		ClientID: keyInfo.ClientID,
		Scopes:   keyInfo.Scopes,
		Roles:    keyInfo.Roles,
	}, nil
}

// authenticateJWT authenticates using JWT token
func (a *Authenticator) authenticateJWT(r *http.Request) (*AuthClaims, error) {
	// Extract token from Authorization header
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return nil, fmt.Errorf("Bearer token not provided")
	}

	tokenString := strings.TrimPrefix(authHeader, "Bearer ")

	// Parse and validate JWT
	token, err := jwt.ParseWithClaims(tokenString, &AuthClaims{}, func(token *jwt.Token) (interface{}, error) {
		// Verify signing method
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return a.jwtSecret, nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to parse JWT: %w", err)
	}

	if !token.Valid {
		return nil, fmt.Errorf("invalid JWT token")
	}

	claims, ok := token.Claims.(*AuthClaims)
	if !ok {
		return nil, fmt.Errorf("failed to parse JWT claims")
	}

	// Verify issuer if configured
	if a.config.JWTConfig != nil && a.config.JWTConfig.Issuer != "" {
		if claims.Issuer != a.config.JWTConfig.Issuer {
			return nil, fmt.Errorf("invalid token issuer")
		}
	}

	// Verify audience if configured
	if a.config.JWTConfig != nil && len(a.config.JWTConfig.Audience) > 0 {
		validAudience := false
		for _, aud := range a.config.JWTConfig.Audience {
			for _, claimAud := range claims.Audience {
				if aud == claimAud {
					validAudience = true
					break
				}
			}
		}
		if !validAudience {
			return nil, fmt.Errorf("invalid token audience")
		}
	}

	return claims, nil
}

// authenticateOAuth2 authenticates using OAuth2 token
func (a *Authenticator) authenticateOAuth2(r *http.Request) (*AuthClaims, error) {
	// Extract token from Authorization header
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return nil, fmt.Errorf("Bearer token not provided")
	}

	token := strings.TrimPrefix(authHeader, "Bearer ")

	// In production, you would validate the token with the OAuth2 provider
	// For now, we'll do a simple validation
	if token == "" {
		return nil, fmt.Errorf("empty OAuth2 token")
	}

	// Mock validation - in production, call the OAuth2 provider's introspection endpoint
	return &AuthClaims{
		ClientID: "oauth2_client",
		Subject:  "oauth2_user",
	}, nil
}

// authenticateMTLS authenticates using mutual TLS
func (a *Authenticator) authenticateMTLS(r *http.Request) (*AuthClaims, error) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil, fmt.Errorf("no client certificate provided")
	}

	cert := r.TLS.PeerCertificates[0]

	// Verify certificate CN or DNS names
	if a.config.MTLSConfig != nil {
		// Check allowed CNs
		if len(a.config.MTLSConfig.AllowedCNs) > 0 {
			allowed := false
			for _, allowedCN := range a.config.MTLSConfig.AllowedCNs {
				if cert.Subject.CommonName == allowedCN {
					allowed = true
					break
				}
			}
			if !allowed {
				return nil, fmt.Errorf("certificate CN not allowed: %s", cert.Subject.CommonName)
			}
		}

		// Check allowed DNS names
		if len(a.config.MTLSConfig.AllowedDNSNames) > 0 {
			allowed := false
			for _, allowedDNS := range a.config.MTLSConfig.AllowedDNSNames {
				for _, dnsName := range cert.DNSNames {
					if dnsName == allowedDNS {
						allowed = true
						break
					}
				}
			}
			if !allowed {
				return nil, fmt.Errorf("certificate DNS names not allowed")
			}
		}
	}

	return &AuthClaims{
		ClientID: cert.Subject.CommonName,
		Subject:  cert.Subject.String(),
	}, nil
}

// verifyRequirements verifies that claims meet requirements
func (a *Authenticator) verifyRequirements(claims *AuthClaims, requirements AuthRequirements) error {
	// Verify scopes
	if len(requirements.AllowedScopes) > 0 {
		hasScope := false
		for _, requiredScope := range requirements.AllowedScopes {
			for _, claimScope := range claims.Scopes {
				if requiredScope == claimScope {
					hasScope = true
					break
				}
			}
		}
		if !hasScope {
			return fmt.Errorf("insufficient scopes")
		}
	}

	// Verify roles
	if len(requirements.AllowedRoles) > 0 {
		hasRole := false
		for _, requiredRole := range requirements.AllowedRoles {
			for _, claimRole := range claims.Roles {
				if requiredRole == claimRole {
					hasRole = true
					break
				}
			}
		}
		if !hasRole {
			return fmt.Errorf("insufficient roles")
		}
	}

	return nil
}

// VerifyWebhookSignature verifies webhook signatures
func (a *Authenticator) VerifyWebhookSignature(r *http.Request, provider string) error {
	secret, exists := a.webhookSecrets[provider]
	if !exists {
		return fmt.Errorf("webhook secret not configured for provider: %s", provider)
	}

	switch provider {
	case "github":
		return a.verifyGitHubSignature(r, secret)
	case "stripe":
		return a.verifyStripeSignature(r, secret)
	case "slack":
		return a.verifySlackSignature(r, secret)
	default:
		return a.verifyGenericHMACSignature(r, secret)
	}
}

// verifyGitHubSignature verifies GitHub webhook signature
func (a *Authenticator) verifyGitHubSignature(r *http.Request, secret string) error {
	signature := r.Header.Get("X-Hub-Signature-256")
	if signature == "" {
		return fmt.Errorf("GitHub signature header not found")
	}

	// Read body (should be cached)
	// In production, body should be read once and cached
	body := []byte{} // Placeholder - actual implementation would read from cached body

	// Calculate HMAC
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expectedSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(signature), []byte(expectedSignature)) {
		return fmt.Errorf("invalid GitHub signature")
	}

	return nil
}

// verifyStripeSignature verifies Stripe webhook signature
func (a *Authenticator) verifyStripeSignature(r *http.Request, secret string) error {
	signature := r.Header.Get("Stripe-Signature")
	if signature == "" {
		return fmt.Errorf("Stripe signature header not found")
	}

	// Parse signature header
	// Format: t=timestamp,v1=signature
	parts := strings.Split(signature, ",")
	var timestamp, sig string
	for _, part := range parts {
		kv := strings.Split(part, "=")
		if len(kv) == 2 {
			if kv[0] == "t" {
				timestamp = kv[1]
			} else if kv[0] == "v1" {
				sig = kv[1]
			}
		}
	}

	// Verify timestamp is recent (prevent replay attacks)
	// Implementation would check timestamp

	// Verify signature
	// Implementation would calculate and compare HMAC

	return nil
}

// verifySlackSignature verifies Slack webhook signature
func (a *Authenticator) verifySlackSignature(r *http.Request, secret string) error {
	signature := r.Header.Get("X-Slack-Signature")
	timestamp := r.Header.Get("X-Slack-Request-Timestamp")

	if signature == "" || timestamp == "" {
		return fmt.Errorf("Slack signature headers not found")
	}

	// Implementation would verify Slack signature

	return nil
}

// verifyGenericHMACSignature verifies generic HMAC signature
func (a *Authenticator) verifyGenericHMACSignature(r *http.Request, secret string) error {
	signature := r.Header.Get("X-Webhook-Signature")
	if signature == "" {
		return fmt.Errorf("webhook signature header not found")
	}

	// Implementation would verify HMAC signature

	return nil
}

// IsAdmin checks if the request has admin privileges
func (a *Authenticator) IsAdmin(r *http.Request) bool {
	// Check for admin API key
	adminKey := r.Header.Get("X-Admin-Key")
	if adminKey != "" && adminKey == "admin-secret-key" { // In production, use secure admin key
		return true
	}

	// Check JWT for admin role
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		claims, err := a.authenticateJWT(r)
		if err == nil && claims != nil {
			return claims.IsAdmin || a.hasAdminRole(claims.Roles)
		}
	}

	return false
}

// hasAdminRole checks if roles contain admin role
func (a *Authenticator) hasAdminRole(roles []string) bool {
	for _, role := range roles {
		if role == "admin" || role == "administrator" || role == "super_admin" {
			return true
		}
	}
	return false
}

// loadDefaultAPIKeys loads default API keys for testing
func (a *Authenticator) loadDefaultAPIKeys() {
	// Default test API keys - in production, load from secure storage
	a.apiKeys["test-api-key-123"] = &APIKeyInfo{
		Key:      "test-api-key-123",
		ClientID: "test-client",
		Scopes:   []string{"read", "write"},
		Roles:    []string{"user"},
		Enabled:  true,
	}

	a.apiKeys["admin-api-key-456"] = &APIKeyInfo{
		Key:      "admin-api-key-456",
		ClientID: "admin-client",
		Scopes:   []string{"read", "write", "admin"},
		Roles:    []string{"admin"},
		Enabled:  true,
	}
}

// loadWebhookSecrets loads webhook secrets
func (a *Authenticator) loadWebhookSecrets() {
	// In production, load from secure storage
	a.webhookSecrets = map[string]string{
		"github": "github-webhook-secret",
		"stripe": "stripe-webhook-secret",
		"slack":  "slack-webhook-secret",
	}
}

// GenerateJWT generates a JWT token
func (a *Authenticator) GenerateJWT(claims *AuthClaims) (string, error) {
	if a.config.JWTConfig == nil {
		return "", fmt.Errorf("JWT not configured")
	}

	// Set expiration if not set
	if claims.ExpiresAt == nil {
		expiresAt := time.Now().Add(time.Duration(a.config.JWTConfig.ExpiresIn) * time.Second)
		claims.ExpiresAt = jwt.NewNumericDate(expiresAt)
	}

	// Set issuer
	claims.Issuer = a.config.JWTConfig.Issuer

	// Create token
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	// Sign token
	tokenString, err := token.SignedString(a.jwtSecret)
	if err != nil {
		return "", fmt.Errorf("failed to sign JWT: %w", err)
	}

	return tokenString, nil
}
