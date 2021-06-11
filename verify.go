package httpsig

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"time"
)

type verImpl struct {
	w      io.Writer
	verify func([]byte) error
}

type verHolder struct {
	alg      string
	verifier func() verImpl
}

type verifier struct {
	keys map[string]verHolder

	// For testing
	nowFunc func() time.Time
}

// XXX: note about fail fast.
func (v *verifier) Verify(msg *message) error {
	sigHdr := msg.Header.Get("Signature")
	if sigHdr == "" {
		return notSignedError
	}

	paramHdr := msg.Header.Get("Signature-Input")
	if paramHdr == "" {
		return notSignedError
	}

	sigParts := strings.Split(sigHdr, ", ")
	paramParts := strings.Split(paramHdr, ", ")

	if len(sigParts) != len(paramParts) {
		return malformedSignatureError
	}

	// TODO: could be smarter about selecting the sig to verify, eg based
	// on algorithm
	var sigID string
	var params *signatureParams
	for _, p := range paramParts {
		pParts := strings.SplitN(p, "=", 2)
		if len(pParts) != 2 {
			return malformedSignatureError
		}

		candidate, err := parseSignatureInput(pParts[1])
		if err != nil {
			return malformedSignatureError
		}

		if _, ok := v.keys[candidate.keyID]; ok {
			sigID = pParts[0]
			params = candidate
			break
		}
	}

	if params == nil {
		return unknownKeyError
	}

	var signature string
	for _, s := range sigParts {
		sParts := strings.SplitN(s, "=", 2)
		if len(sParts) != 2 {
			return malformedSignatureError
		}

		if sParts[0] == sigID {
			// TODO: error if not surrounded by colons
			signature = strings.Trim(sParts[1], ":")
			break
		}
	}

	if signature == "" {
		return malformedSignatureError
	}

	ver := v.keys[params.keyID]
	if ver.alg != "" && params.alg != "" && ver.alg != params.alg {
		return algMismatchError
	}

	// verify signature. if invalid, error
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return malformedSignatureError
	}

	verifier := ver.verifier()

	//TODO: skip the buffer.

	var b bytes.Buffer

	// canonicalize headers
	// TODO: wrap the errors within
	for _, h := range params.items {
		// optionally canonicalize request path via magic string
		if h == "@request-target" {
			err := canonicalizeRequestTarget(&b, msg.Method, msg.URL)
			if err != nil {
				return err
			}
			continue
		}

		err := canonicalizeHeader(&b, h, msg.Header)
		if err != nil {
			return err
		}
	}

	verifier.w.Write(b.Bytes())
	canonicalizeSignatureParams(verifier.w, params)

	err = verifier.verify(sig)
	if err != nil {
		return invalidSignatureError
	}

	// TODO: could put in some wiggle room
	if params.expires != nil && params.expires.After(time.Now()) {
		return signatureExpiredError
	}

	return nil
}

// XXX use vice here too.

var (
	notSignedError          = errors.New("signature headers not found")
	malformedSignatureError = errors.New("unable to parse signature headers")
	unknownKeyError         = errors.New("unknown key id")
	algMismatchError        = errors.New("algorithm mismatch for key id")
	signatureExpiredError   = errors.New("signature expired")
	invalidSignatureError   = errors.New("invalid signature")
)

func IsNotSignedError(err error) bool          { return errors.Is(err, notSignedError) }
func IsMalformedSignatureError(err error) bool { return errors.Is(err, malformedSignatureError) }
func IsUnknownKeyError(err error) bool         { return errors.Is(err, unknownKeyError) }
func IsAlgMismatchError(err error) bool        { return errors.Is(err, algMismatchError) }
func IsSignatureExpiredError(err error) bool   { return errors.Is(err, signatureExpiredError) }
func IsInvalidSignatureError(err error) bool   { return errors.Is(err, invalidSignatureError) }

func verifyRsaPssSha512(pk *rsa.PublicKey) verHolder {
	return verHolder{
		alg: "rsa-pss-sha512",
		verifier: func() verImpl {
			h := sha512.New()

			return verImpl{
				w: h,
				verify: func(s []byte) error {
					b := h.Sum(nil)

					return rsa.VerifyPSS(pk, crypto.SHA512, b, s, nil)
				},
			}
		},
	}
}

func verifyEccP256(pk *ecdsa.PublicKey) verHolder {
	return verHolder{
		alg: "ecdsa-p256-sha256",
		verifier: func() verImpl {
			h := sha256.New()

			return verImpl{
				w: h,
				verify: func(s []byte) error {
					b := h.Sum(nil)

					if !ecdsa.VerifyASN1(pk, b, s) {
						return invalidSignatureError
					}

					return nil
				},
			}
		},
	}
}

func verifyHmacSha256(secret []byte) verHolder {
	// TODO: add alg
	return verHolder{
		alg: "hmac-sha256",
		verifier: func() verImpl {
			h := hmac.New(sha256.New, secret)

			return verImpl{
				w: h,
				verify: func(in []byte) error {
					if !hmac.Equal(in, h.Sum(nil)) {
						return invalidSignatureError
					}
					return nil
				},
			}
		},
	}
}