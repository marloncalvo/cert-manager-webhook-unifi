package unifi

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"golang.org/x/net/publicsuffix"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"sigs.k8s.io/external-dns/endpoint"

	"go.uber.org/zap"
)

type ClientURLs struct {
	Login   string
	Records string
}

// httpClient is the DNS provider client.
type httpClient struct {
	*Config
	*http.Client
	csrf       string
	ClientURLs *ClientURLs
}

const (
	unifiLoginPath          = "%s/api/auth/login"
	unifiLoginPathExternal  = "%s/api/login"
	unifiRecordPath         = "%s/proxy/network/v2/api/site/%s/static-dns/%s"
	unifiRecordPathExternal = "%s/v2/api/site/%s/static-dns/%s"
)

// newUnifiClient creates a new DNS provider client and logs in to store cookies.
func newUnifiClient(config *Config) (*httpClient, error) {
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, err
	}

	// Create the HTTP client
	client := &httpClient{
		Config: config,
		Client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: config.SkipTLSVerify},
			},
			Jar: jar,
		},
		ClientURLs: &ClientURLs{
			Login:   unifiLoginPath,
			Records: unifiRecordPath,
		},
	}

	if config.ExternalController {
		client.ClientURLs.Login = unifiLoginPathExternal
		client.ClientURLs.Records = unifiRecordPathExternal
	}

	if err := client.login(); err != nil {
		return nil, err
	}

	return client, nil
}

// login performs a login request to the UniFi controller.
func (c *httpClient) login() error {
	jsonBody, err := json.Marshal(Login{
		Username: c.Config.User,
		Password: c.Config.Password,
		Remember: true,
	})
	if err != nil {
		return err
	}

	// Perform the login request
	resp, err := c.doRequest(
		http.MethodPost,
		FormatUrl(c.ClientURLs.Login, c.Config.Host),
		bytes.NewBuffer(jsonBody),
	)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	// Check if the login was successful
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		log.Println("login failed", zap.String("status", resp.Status), zap.String("response", string(respBody)))
		return fmt.Errorf("login failed: %s", resp.Status)
	}

	// Retrieve CSRF token from the response headers
	if csrf := resp.Header.Get("x-csrf-token"); csrf != "" {
		c.csrf = resp.Header.Get("x-csrf-token")
	}
	return nil
}

func (c *httpClient) doRequest(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, path, body)
	if err != nil {
		return nil, err
	}

	c.setHeaders(req)

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}

	if csrf := resp.Header.Get("X-CSRF-Token"); csrf != "" {
		c.csrf = csrf
	}

	// If the status code is 401, re-login and retry the request
	if resp.StatusCode == http.StatusUnauthorized {
		log.Println("received 401 unauthorized, attempting to re-login")
		if err := c.login(); err != nil {
			log.Println("re-login failed", zap.Error(err))
			return nil, err
		}
		// Update the headers with new CSRF token
		c.setHeaders(req)

		// Retry the request
		log.Println("retrying request after re-login")

		resp, err = c.Client.Do(req)
		if err != nil {
			log.Println("Retry request failed", zap.Error(err))
			return nil, err
		}
	}

	// It is unknown at this time if the UniFi API returns anything other than 200 for these types of requests.
	if resp.StatusCode != http.StatusOK {
		body, bodyErr := io.ReadAll(io.LimitReader(resp.Body, 512))
		if bodyErr != nil {
			return nil, bodyErr
		}

		var apiError UnifiErrorResponse
		if err := json.Unmarshal(body, &apiError); err != nil {
			return nil, fmt.Errorf("failed to decode json: %w", err)
		}

		return nil, fmt.Errorf("%s request to %s returned %d: %s", method, path, resp.StatusCode, apiError.Message)
	}

	return resp, nil
}

// GetEndpoints retrieves the list of DNS records from the UniFi controller.
func (c *httpClient) GetEndpoints() ([]DNSRecord, error) {
	resp, err := c.doRequest(
		http.MethodGet,
		FormatUrl(c.ClientURLs.Records, c.Config.Host, c.Config.Site),
		nil,
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var records []DNSRecord
	if err = json.NewDecoder(resp.Body).Decode(&records); err != nil {
		log.Println("Failed to decode response", zap.Error(err))
		return nil, err
	}

	// Loop through records to modify SRV type
	for i, record := range records {
		if record.RecordType != "SRV" {
			continue
		}

		// Modify the Target for SRV records
		records[i].Value = fmt.Sprintf("%d %d %d %s",
			*record.Priority,
			*record.Weight,
			*record.Port,
			record.Value,
		)
		records[i].Priority = nil
		records[i].Weight = nil
		records[i].Port = nil
	}

	log.Println("retrieved records", zap.Int("count", len(records)))
	return records, nil
}

// CreateEndpoint creates a new DNS record in the UniFi controller.
// Future Kash: We don't support multiple targets per dns name and need to effectively create x records.
func (c *httpClient) CreateEndpoint(endpoint *endpoint.Endpoint) (*DNSRecord, error) {
	record := DNSRecord{
		Enabled:    true,
		Key:        endpoint.DNSName,
		RecordType: endpoint.RecordType,
		TTL:        endpoint.RecordTTL,
		Value:      endpoint.Targets[0],
	}

	if endpoint.RecordType == "SRV" {
		record.Priority = new(int)
		record.Weight = new(int)
		record.Port = new(int)

		if _, err := fmt.Sscanf(endpoint.Targets[0], "%d %d %d %s", record.Priority, record.Weight, record.Port, &record.Value); err != nil {
			return nil, err
		}
	}

	jsonBody, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}

	resp, err := c.doRequest(
		http.MethodPost,
		FormatUrl(c.ClientURLs.Records, c.Config.Host, c.Config.Site),
		bytes.NewReader(jsonBody),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var createdRecord DNSRecord
	if err = json.NewDecoder(resp.Body).Decode(&createdRecord); err != nil {
		return nil, err
	}

	return &createdRecord, nil
}

// DeleteEndpoint deletes a DNS record from the UniFi controller.
func (c *httpClient) DeleteEndpoint(endpoint *endpoint.Endpoint) error {
	lookup, err := c.lookupIdentifier(endpoint.DNSName, endpoint.RecordType, endpoint.Targets)
	if err != nil {
		return err
	}

	deleteURL := FormatUrl(c.ClientURLs.Records, c.Config.Host, c.Config.Site, lookup.ID)

	_, err = c.doRequest(
		http.MethodDelete,
		deleteURL,
		nil,
	)
	if err != nil {
		return err
	}

	return nil
}

// lookupIdentifier finds the ID of a DNS record in the UniFi controller.
func (c *httpClient) lookupIdentifier(key, recordType string, recordValue []string) (*DNSRecord, error) {
	log.Println("Looking up identifier", zap.String("key", key), zap.String("recordType", recordType))
	records, err := c.GetEndpoints()
	if err != nil {
		return nil, err
	}

	var retRecord *DNSRecord

	if len(recordValue) == 0 {
		for _, r := range records {
			if r.Key == key && r.RecordType == recordType {
				return &r, nil
			}
		}
	} else {
		for _, value := range recordValue {
			for _, r := range records {
				if r.Key == key && r.RecordType == recordType && r.Value == value {
					retRecord = &r
				}
			}
		}
	}

	if retRecord == nil {
		return nil, fmt.Errorf("record not found: %s", key)
	}

	return retRecord, nil
}

// setHeaders sets the headers for the HTTP request.
func (c *httpClient) setHeaders(req *http.Request) {
	// Add the saved CSRF header.
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Content-Type", "application/json; charset=utf-8")
}