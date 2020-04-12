package models

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

type JWK map[string]string

//DoRequest - executes a request and does refreshing automatically
func (l *MesheryRemoteProvider) DoRequest(req *http.Request, tokenString string) (*http.Response, error) {
	resp, err := l.doRequestHelper(req, tokenString)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		logrus.Errorf("Trying after refresh")
		newToken, err := l.refreshToken(tokenString)
		if err != nil {
			logrus.Errorf("error doing token : %v", err.Error())
			return nil, err
		}
		return l.doRequestHelper(req, newToken)
	}
	return resp, err
}

// refreshToken - takes a tokenString and returns a new tokenString
func (l *MesheryRemoteProvider) refreshToken(tokenString string) (string, error) {
	l.TokenStoreMut.Lock()
	defer l.TokenStoreMut.Unlock()
	newTokenString := l.TokenStore[tokenString]
	if newTokenString != "" {
		return newTokenString, nil
	}
	bd := map[string]string{
		tokenName: tokenString,
	}
	jsonString, err := json.Marshal(bd)
	if err != nil {
		logrus.Errorf("error refreshing token : %v", err.Error())
		return "", err
	}
	r, err := http.Post(l.SaaSBaseURL+"/refresh", "application/json; charset=utf-8", bytes.NewReader(jsonString))
	defer r.Body.Close()
	var target map[string]string
	err = json.NewDecoder(r.Body).Decode(&target)
	if err != nil {
		logrus.Errorf("error refreshing token : %v", err.Error())
		return "", err
	}
	l.TokenStore[tokenString] = target[tokenName]
	time.AfterFunc(5*time.Minute, func() {
		logrus.Infof("deleting old ts")
		delete(l.TokenStore, tokenString)
	})
	return target[tokenName], nil
}

func (l *MesheryRemoteProvider) doRequestHelper(req *http.Request, tokenString string) (*http.Response, error) {
	token, err := l.DecodeTokenData(tokenString)
	if err != nil {
		logrus.Errorf("Error performing the request, %s", err.Error())
		return nil, err
	}
	c := &http.Client{}
	req.Header.Set("Authorization", fmt.Sprintf("bearer %s", token.AccessToken))
	resp, err := c.Do(req)
	if err != nil {
		logrus.Errorf("Error performing the request, %s", err.Error())
		return nil, err
	}
	return resp, nil
}

// GetToken - extracts token form a request
func (l *MesheryRemoteProvider) GetToken(req *http.Request) (string, error) {
	ck, err := req.Cookie(tokenName)
	if err != nil {
		logrus.Errorf("Error in getting the token, %s", err.Error())
		return "", err
	}
	return ck.Value, nil
}

// DecodeTokenData - Decodes a tokenString to a token
func (l *MesheryRemoteProvider) DecodeTokenData(tokenStringB64 string) (*oauth2.Token, error) {
	var token oauth2.Token
	// logrus.Debugf("Token string %s", tokenStringB64)
	tokenString, err := base64.RawStdEncoding.DecodeString(tokenStringB64)
	if err != nil {
		logrus.Errorf("token decode error : %s", err.Error())
		return nil, err
	}
	err = json.Unmarshal(tokenString, &token)
	if err != nil {
		logrus.Errorf("token decode error : %s", err.Error())
		return nil, err
	}
	return &token, nil
}

// UpdateJWKs - Updates Keys to the JWKS
func (l *MesheryRemoteProvider) UpdateJWKs() error {
	resp, err := http.Get(l.SaaSBaseURL + "/keys")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err != nil {
		return err
	}
	jsonDataFromHttp, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	jwksJSON := map[string][]map[string]string{}
	json.Unmarshal([]byte(jsonDataFromHttp), &jwksJSON)

	jwks := jwksJSON["keys"]

	l.Keys = jwks

	return nil
}

// GetJWK - Takes a keyId and returns the JWK
func (l *MesheryRemoteProvider) GetJWK(kid string) (JWK, error) {
	for _, key := range l.Keys {
		if key["kid"] == kid {
			return key, nil
		}
	}
	err := l.UpdateJWKs()
	if err != nil {
		return nil, err
	}
	for _, key := range l.Keys {
		if key["kid"] == kid {
			return key, nil
		}
	}
	return nil, fmt.Errorf("Key not found")
}

// GenerateKey - generate the actual key from the JWK
func (l *MesheryRemoteProvider) GenerateKey(jwk JWK) (*rsa.PublicKey, error) {

	// decode the base64 bytes for n
	nb, err := base64.RawURLEncoding.DecodeString(jwk["n"])
	if err != nil {
		logrus.Fatal(err)
		return nil, err
	}

	e := 0
	// The default exponent is usually 65537, so just compare the
	// base64 for [1,0,1] or [0,1,0,1]
	if jwk["e"] == "AQAB" || jwk["e"] == "AAEAAQ" {
		e = 65537
	} else {
		// need to decode "e" as a big-endian int
		logrus.Fatal("need to deocde e:", jwk["e"])
		return nil, err
	}

	pk := &rsa.PublicKey{
		N: new(big.Int).SetBytes(nb),
		E: e,
	}

	der, err := x509.MarshalPKIXPublicKey(pk)
	if err != nil {
		logrus.Fatal(err)
		return nil, err
	}

	block := &pem.Block{
		Type:  "RSA PUBLIC KEY",
		Bytes: der,
	}

	var out bytes.Buffer
	pem.Encode(&out, block)
	return jwt.ParseRSAPublicKeyFromPEM(out.Bytes())
}

// VerifyToken - verifies the validity of a tokenstring
func (l *MesheryRemoteProvider) VerifyToken(tokenString string) (*jwt.MapClaims, error) {
	dtoken, err := l.DecodeTokenData(tokenString)
	if err != nil {
		logrus.Error("E1")
		return nil, err
	}
	tokenString = dtoken.AccessToken
	tokenUP, _, err := new(jwt.Parser).ParseUnverified(tokenString, jwt.MapClaims{})
	if err != nil {
		logrus.Error("2")
		return nil, err
	}
	kid := tokenUP.Header["kid"].(string)
	keyJSON, err := l.GetJWK(kid)
	if err != nil {
		logrus.Error("3")
		return nil, err
	}
	key, err := l.GenerateKey(keyJSON)
	if err != nil {
		return nil, err
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return key, nil
	})

	if err != nil {
		return nil, err
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("Error parsing claims")
	}
	return &claims, nil
}
