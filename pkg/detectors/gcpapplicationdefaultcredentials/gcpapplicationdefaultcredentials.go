package gcpapplicationdefaultcredentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	regexp "github.com/wasilibs/go-re2"

	"github.com/trufflesecurity/trufflehog/v3/pkg/common"
	"github.com/trufflesecurity/trufflehog/v3/pkg/detectors"
	"github.com/trufflesecurity/trufflehog/v3/pkg/pb/detectorspb"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type Scanner struct {
	client *http.Client
}

// Ensure the Scanner satisfies the interface at compile time.
var _ detectors.Detector = (*Scanner)(nil)

var (
	defaultClient = common.SaneHttpClient()

	keyPat = regexp.MustCompile(`\{[^{]+client_secret[^}]+\}`)
)

type gcpApplicationDefaultCredentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	Type         string `json:"type"`
}

// Keywords are used for efficiently pre-filtering chunks.
// Use identifiers in the secret preferably, or the provider name.
func (s Scanner) Keywords() []string {
	return []string{"client_secret"}
}

// FromData will find and optionally verify Gcpapplicationdefaultcredentials secrets in a given set of bytes.
func (s Scanner) FromData(ctx context.Context, verify bool, data []byte) (results []detectors.Result, err error) {
	dataStr := string(data)

	matches := keyPat.FindAllString(dataStr, -1)

	for _, match := range matches {
		key := match

		// Detect keys by unmarshalling the data.
		creds := gcpApplicationDefaultCredentials{}
		err := json.Unmarshal([]byte(key), &creds)
		if err != nil {
			continue
		}

		detectedClientID, _, _ := strings.Cut(creds.ClientID, ".")

		s1 := detectors.Result{
			DetectorType: detectorspb.DetectorType_GCPApplicationDefaultCredentials,
			Raw:          []byte(detectedClientID),
		}

		if verify {
			client := s.client
			if client == nil {
				client = defaultClient
			}

			// Use marshalled credential to verify if the found key is active
			credBytes, _ := json.Marshal(creds)
			isVerified, extraData, verificationErr := verifyMatch(ctx, client, string(credBytes))
			s1.Verified = isVerified
			s1.ExtraData = extraData
			s1.SetVerificationError(verificationErr, match)
		}

		// This function will check false positives for common test words, but also it will make sure the key appears 'random' enough to be a real key.
		if !s1.Verified && detectors.IsKnownFalsePositive(string(s1.Raw), detectors.DefaultFalsePositives, true) {
			continue
		}

		results = append(results, s1)
	}

	return
}

func verifyMatch(ctx context.Context, client *http.Client, token string) (bool, map[string]string, error) {
	// First load the credential from the found key
	credentials, err := google.CredentialsFromJSON(ctx, []byte(token), "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return false, nil, err
	}

	// Credential not loaded. Not sure this can happend but it should be labeled unverified.
	if credentials == nil {
		return false, nil, nil
	}

	// Get token from the credentials
	gcpToken, err := credentials.TokenSource.Token()

	if err != nil {
		// Return verification error if the error is temporary
		// See https://pkg.go.dev/golang.org/x/oauth2/google#AuthenticationError.Temporary for details
		var temporaryError *(google.AuthenticationError)
		if errors.As(err, &temporaryError) {
			if err.(*google.AuthenticationError).Temporary() {
				return false, nil, err
			}
		}
		return false, nil, nil
	}

	// Return verification error if the retrieved token is invalid
	if !gcpToken.Valid() {
		return false, nil, err
	}

	// Build request to call an IAM endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://iam.googleapis.com/v1/roles", nil)
	if err != nil {
		return false, nil, nil
	}

	// If we are not using a faketransport, leave it as is because the test wants to modify the response. Otherwise, set the retrieved token to the client.
	if _, ok := client.Transport.(common.FakeTransport); !ok {
		client.Transport = &oauth2.Transport{
			Source: credentials.TokenSource,
		}
	}

	res, err := client.Do(req)
	if err != nil {
		return false, nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()

	if res.StatusCode >= 200 && res.StatusCode < 300 {
		// If the endpoint returns useful information, we can return it as a map.
		return true, nil, nil
	} else if res.StatusCode == 401 {
		// The secret is determinately not verified (nothing to do)
		return false, nil, nil
	} else {
		err = fmt.Errorf("unexpected HTTP response status %d", res.StatusCode)
		return false, nil, err
	}
}

func (s Scanner) Type() detectorspb.DetectorType {
	return detectorspb.DetectorType_GCPApplicationDefaultCredentials
}
