package auth

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/instance"
	jwt "gopkg.in/dgrijalva/jwt-go.v3"
)

const (
	// ClientSecretLen is the number of random bytes used for generating the client secret
	ClientSecretLen = 24

	// RegistrationTokenAudience is the audience field of JWT for registration tokens
	RegistrationTokenAudience = "registration"

	// AccessTokenAudience is the audience field of JWT for access tokens
	AccessTokenAudience = "access"

	// RefreshTokenAudience is the audience field of JWT for refresh tokens
	RefreshTokenAudience = "refresh"
)

// Client is a struct for OAuth2 client. Most of the fields are described in
// the OAuth 2.0 Dynamic Client Registration Protocol. The exception is
// `client_kind`, and it is an optional field.
// See https://tools.ietf.org/html/rfc7591
//
// CouchID and ClientID are the same. They are just two ways to serialize to
// JSON, one for CouchDB and the other for the Dynamic Client Registration
// Protocol.
type Client struct {
	CouchID  string `json:"_id,omitempty"`  // Generated by CouchDB
	CouchRev string `json:"_rev,omitempty"` // Generated by CouchDB

	ClientID          string `json:"client_id,omitempty"`       // Same as CouchID
	ClientSecret      string `json:"client_secret"`             // Generated by the server
	SecretExpiresAt   int    `json:"client_secret_expires_at"`  // Forced by the server to 0 (no expiration)
	RegistrationToken string `json:"registration_access_token"` // Generated by the server

	RedirectURIs    []string `json:"redirect_uris"`              // Declared by the client (mandatory)
	GrantTypes      []string `json:"grant_types"`                // Forced by the server to ["authorization_code", "refresh_token"]
	ResponseTypes   []string `json:"response_types"`             // Forced by the server to ["code"]
	ClientName      string   `json:"client_name"`                // Declared by the client (mandatory)
	ClientKind      string   `json:"client_kind,omitempty"`      // Declared by the client (optional, can be "desktop", "mobile", "browser", etc.)
	ClientURI       string   `json:"client_uri,omitempty"`       // Declared by the client (optional)
	LogoURI         string   `json:"logo_uri,omitempty"`         // Declared by the client (optional)
	PolicyURI       string   `json:"policy_uri,omitempty"`       // Declared by the client (optional)
	SoftwareID      string   `json:"software_id"`                // Declared by the client (mandatory)
	SoftwareVersion string   `json:"software_version,omitempty"` // Declared by the client (optional)
}

// ID returns the client qualified identifier
func (c *Client) ID() string { return c.CouchID }

// Rev returns the client revision
func (c *Client) Rev() string { return c.CouchRev }

// DocType returns the client document type
func (c *Client) DocType() string { return consts.OAuthClients }

// SetID changes the client qualified identifier
func (c *Client) SetID(id string) { c.CouchID = id }

// SetRev changes the client revision
func (c *Client) SetRev(rev string) { c.CouchRev = rev }

func (c *Client) transformIDAndRev() {
	c.ClientID = c.CouchID
	c.CouchID = ""
	c.CouchRev = ""
}

// FindClient loads a client from the database
func FindClient(i *instance.Instance, id string) (Client, error) {
	var c Client
	if err := couchdb.GetDoc(i, consts.OAuthClients, id, &c); err != nil {
		log.Errorf("Impossible to find the client %s: %s", id, err)
		return c, err
	}
	return c, nil
}

// ClientRegistrationError is a Client Registration Error Response, as described
// in the Client Dynamic Registration Protocol
// See https://tools.ietf.org/html/rfc7591#section-3.2.2 for errors
type ClientRegistrationError struct {
	Code        int    `json:"-"`
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func (c *Client) checkMandatoryFields(i *instance.Instance) *ClientRegistrationError {
	if len(c.RedirectURIs) == 0 {
		return &ClientRegistrationError{
			Code:        http.StatusBadRequest,
			Error:       "invalid_redirect_uri",
			Description: "redirect_uris is mandatory",
		}
	}
	for _, redirectURI := range c.RedirectURIs {
		u, err := url.Parse(redirectURI)
		if err != nil ||
			u.Host == i.Domain ||
			u.Fragment != "" {
			return &ClientRegistrationError{
				Code:        http.StatusBadRequest,
				Error:       "invalid_redirect_uri",
				Description: fmt.Sprintf("%s is invalid", redirectURI),
			}
		}
	}
	if c.ClientName == "" {
		return &ClientRegistrationError{
			Code:        http.StatusBadRequest,
			Error:       "invalid_client_metadata",
			Description: "client_name is mandatory",
		}
	}
	if c.SoftwareID == "" {
		return &ClientRegistrationError{
			Code:        http.StatusBadRequest,
			Error:       "invalid_client_metadata",
			Description: "software_id is mandatory",
		}
	}

	return nil
}

// Create is a function that sets some fields, and then save it in Couch.
func (c *Client) Create(i *instance.Instance) *ClientRegistrationError {
	if err := c.checkMandatoryFields(i); err != nil {
		return err
	}

	c.CouchID = ""
	c.CouchRev = ""
	c.ClientID = ""
	secret := crypto.GenerateRandomBytes(ClientSecretLen)
	c.ClientSecret = string(crypto.Base64Encode(secret))
	c.SecretExpiresAt = 0
	c.RegistrationToken = ""
	c.GrantTypes = []string{"authorization_code", "refresh_token"}
	c.ResponseTypes = []string{"code"}

	if err := couchdb.CreateDoc(i, c); err != nil {
		return &ClientRegistrationError{
			Code:  http.StatusInternalServerError,
			Error: "internal_server_error",
		}
	}

	var err error
	c.RegistrationToken, err = crypto.NewJWT(i.OAuthSecret, jwt.StandardClaims{
		Audience: RegistrationTokenAudience,
		Issuer:   i.Domain,
		IssuedAt: time.Now().Unix(),
		Subject:  c.CouchID,
	})
	if err != nil {
		log.Errorf("[oauth] Failed to create the registration access token: %s", err)
		return &ClientRegistrationError{
			Code:  http.StatusInternalServerError,
			Error: "internal_server_error",
		}
	}

	c.transformIDAndRev()
	return nil
}

// Update will update the client metadata
func (c *Client) Update(i *instance.Instance, old *Client) *ClientRegistrationError {
	if c.ClientID != old.CouchID {
		return &ClientRegistrationError{
			Code:        http.StatusBadRequest,
			Error:       "invalid_client_id",
			Description: "client_id is mandatory",
		}
	}

	if err := c.checkMandatoryFields(i); err != nil {
		return err
	}

	switch c.ClientSecret {
	case "":
		c.ClientSecret = old.ClientSecret
	case old.ClientSecret:
		secret := crypto.GenerateRandomBytes(ClientSecretLen)
		c.ClientSecret = string(crypto.Base64Encode(secret))
	default:
		return &ClientRegistrationError{
			Code:        http.StatusBadRequest,
			Error:       "invalid_client_secret",
			Description: "client_secret is invalid",
		}
	}

	c.CouchID = old.CouchID
	c.CouchRev = old.CouchRev
	c.ClientID = ""
	c.SecretExpiresAt = 0
	c.RegistrationToken = ""
	c.GrantTypes = []string{"authorization_code", "refresh_token"}
	c.ResponseTypes = []string{"code"}

	if err := couchdb.UpdateDoc(i, c); err != nil {
		return &ClientRegistrationError{
			Code:  http.StatusInternalServerError,
			Error: "internal_server_error",
		}
	}

	c.transformIDAndRev()
	return nil
}

// AcceptRedirectURI returns true if the given URI matches the registered
// redirect_uris
func (c *Client) AcceptRedirectURI(u string) bool {
	for _, uri := range c.RedirectURIs {
		if u == uri {
			return true
		}
	}
	return false
}

// Claims is used for JWT used in OAuth2 flow
type Claims struct {
	jwt.StandardClaims
	Scope string `json:"scope,omitempty"`
}

// CreateJWT returns a new JSON Web Token for the given instance and audience
func (c *Client) CreateJWT(i *instance.Instance, audience, scope string) (string, error) {
	token, err := crypto.NewJWT(i.OAuthSecret, Claims{
		jwt.StandardClaims{
			Audience: audience,
			Issuer:   i.Domain,
			IssuedAt: crypto.Timestamp(),
			Subject:  c.CouchID,
		},
		scope,
	})
	if err != nil {
		log.Errorf("[oauth] Failed to create the %s token: %s", audience, err)
	}
	return token, err
}

// ValidToken checks that the JWT is valid and returns the associate claims
func (c *Client) ValidToken(i *instance.Instance, audience, token string) (Claims, bool) {
	claims := Claims{}
	if token == "" {
		return claims, false
	}
	if err := crypto.ParseJWT(token, i.OAuthSecret, &claims); err != nil {
		log.Errorf("[oauth] Failed to verify the %s token: %s", audience, err)
		return claims, false
	}
	// Note: the refresh and registration tokens don't expire, no need to check its issue date
	if claims.Audience != audience {
		log.Errorf("[oauth] Unexpected audience for %s token: %s", audience, claims.Audience)
		return claims, false
	}
	if claims.Issuer != i.Domain {
		log.Errorf("[oauth] Expected %s issuer for %s token, but was: %s", audience, i.Domain, claims.Issuer)
		return claims, false
	}
	if claims.Subject != c.CouchID {
		log.Errorf("[oauth] Expected %s subject for %s token, but was: %s", audience, c.CouchID, claims.Subject)
		return claims, false
	}
	return claims, true
}

var (
	_ couchdb.Doc = &Client{}
)
