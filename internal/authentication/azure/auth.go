/*
Copyright 2021 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azure

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"golang.org/x/crypto/pkcs12"

	"github.com/dapr/components-contrib/metadata"
)

// NewEnvironmentSettings returns a new EnvironmentSettings configured for a given Azure resource.
func NewEnvironmentSettings(md map[string]string) (EnvironmentSettings, error) {
	es := EnvironmentSettings{
		Metadata: md,
	}
	azureEnv, err := es.GetAzureEnvironment()
	if err != nil {
		return es, err
	}
	es.AzureEnvironment = azureEnv
	return es, nil
}

// EnvironmentSettings hold settings to authenticate with Azure.
type EnvironmentSettings struct {
	Metadata         map[string]string
	AzureEnvironment *azure.Environment
}

// GetAzureEnvironment returns the Azure environment for a given name.
func (s EnvironmentSettings) GetAzureEnvironment() (*azure.Environment, error) {
	envName, ok := s.GetEnvironment("AzureEnvironment")
	if !ok || envName == "" {
		envName = DefaultAzureEnvironment
	}
	env, err := azure.EnvironmentFromName(envName)
	if err != nil {
		return nil, err
	}

	return &env, err
}

// GetTokenCredential returns an azcore.TokenCredential retrieved from, in order:
// 1. Client credentials
// 2. Client certificate
// 3. MSI
func (s EnvironmentSettings) GetTokenCredential() (azcore.TokenCredential, error) {
	// Create a chain
	var creds []azcore.TokenCredential
	errs := make([]error, 0, 3)

	// 1. Client credentials
	if c, e := s.GetClientCredentials(); e == nil {
		cred, err := c.GetTokenCredential()
		if err == nil {
			creds = append(creds, cred)
		} else {
			errs = append(errs, err)
		}
	}

	// 2. Client certificate
	if c, e := s.GetClientCert(); e == nil {
		cred, err := c.GetTokenCredential()
		if err == nil {
			creds = append(creds, cred)
		} else {
			errs = append(errs, err)
		}
	}

	// 3. MSI
	{
		c := s.GetMSI()
		cred, err := c.GetTokenCredential()
		if err == nil {
			creds = append(creds, cred)
		} else {
			errs = append(errs, err)
		}
	}

	if len(creds) == 0 {
		return nil, fmt.Errorf("no suitable token provider for Azure AD; errors: %w", errors.Join(errs...))
	}
	return azidentity.NewChainedTokenCredential(creds, nil)
}

// GetClientCredentials creates a config object from the available client credentials.
// An error is returned if no certificate credentials are available.
func (s EnvironmentSettings) GetClientCredentials() (CredentialsConfig, error) {
	azureEnv, err := s.GetAzureEnvironment()
	if err != nil {
		return CredentialsConfig{}, err
	}

	clientID, _ := s.GetEnvironment("ClientID")
	clientSecret, _ := s.GetEnvironment("ClientSecret")
	tenantID, _ := s.GetEnvironment("TenantID")

	if clientID == "" || clientSecret == "" || tenantID == "" {
		return CredentialsConfig{}, errors.New("parameters clientId, clientSecret, and tenantId must all be present")
	}

	authorizer := NewCredentialsConfig(clientID, tenantID, clientSecret, azureEnv)

	return authorizer, nil
}

// GetClientCert creates a config object from the available certificate credentials.
// An error is returned if no certificate credentials are available.
func (s EnvironmentSettings) GetClientCert() (CertConfig, error) {
	azureEnv, err := s.GetAzureEnvironment()
	if err != nil {
		return CertConfig{}, err
	}

	certFilePath, certFilePathPresent := s.GetEnvironment("CertificateFile")
	certBytes, certBytesPresent := s.GetEnvironment("Certificate")
	certPassword, _ := s.GetEnvironment("CertificatePassword")
	clientID, _ := s.GetEnvironment("ClientID")
	tenantID, _ := s.GetEnvironment("TenantID")

	if !certFilePathPresent && !certBytesPresent {
		return CertConfig{}, fmt.Errorf("missing client certificate")
	}

	authorizer := NewCertConfig(clientID, tenantID, certFilePath, []byte(certBytes), certPassword, azureEnv)

	return authorizer, nil
}

// GetMSI creates a MSI config object from the available client ID.
func (s EnvironmentSettings) GetMSI() MSIConfig {
	config := NewMSIConfig()
	// This is optional and it's ok if value is empty
	config.ClientID, _ = s.GetEnvironment("ClientID")

	return config
}

// CredentialsConfig provides the options to get a bearer authorizer from client credentials.
type CredentialsConfig struct {
	*auth.ClientCredentialsConfig
}

// NewCredentialsConfig creates an CredentialsConfig object configured to obtain an Authorizer through Client Credentials.
func NewCredentialsConfig(clientID string, tenantID string, clientSecret string, env *azure.Environment) CredentialsConfig {
	return CredentialsConfig{
		&auth.ClientCredentialsConfig{
			ClientSecret: clientSecret,
			ClientID:     clientID,
			TenantID:     tenantID,
			AADEndpoint:  env.ActiveDirectoryEndpoint,
		},
	}
}

// GetTokenCredential returns the azcore.TokenCredential object from the credentials.
func (c CredentialsConfig) GetTokenCredential() (token azcore.TokenCredential, err error) {
	return azidentity.NewClientSecretCredential(c.TenantID, c.ClientID, c.ClientSecret, &azidentity.ClientSecretCredentialOptions{
		ClientOptions: azcore.ClientOptions{
			Cloud: cloud.Configuration{
				ActiveDirectoryAuthorityHost: c.AADEndpoint,
			},
		},
	})
}

// CertConfig provides the options to get a bearer authorizer from a client certificate.
type CertConfig struct {
	*auth.ClientCertificateConfig
	CertificateData []byte
}

// NewCertConfig creates an CertConfig object configured to obtain an Authorizer through Client Credentials, using a certificate.
func NewCertConfig(clientID string, tenantID string, certificatePath string, certificateBytes []byte, certificatePassword string, env *azure.Environment) CertConfig {
	return CertConfig{
		&auth.ClientCertificateConfig{
			CertificatePath:     certificatePath,
			CertificatePassword: certificatePassword,
			ClientID:            clientID,
			TenantID:            tenantID,
			AADEndpoint:         env.ActiveDirectoryEndpoint,
		},
		certificateBytes,
	}
}

// GetTokenCredential returns the azcore.TokenCredential object from client certificate.
func (c CertConfig) GetTokenCredential() (token azcore.TokenCredential, err error) {
	ccc := c.ClientCertificateConfig

	// Certificate data - may be empty here
	data := c.CertificateData

	// If we have a certificate path, load it
	if c.ClientCertificateConfig.CertificatePath != "" {
		var errB error
		data, errB = os.ReadFile(ccc.CertificatePath)
		if errB != nil {
			return nil, fmt.Errorf("failed to read the certificate file (%s): %v", ccc.CertificatePath, errB)
		}
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("certificate is not given")
	}

	// Decode the certificate
	cert, key, err := c.decodeCertificate(data, c.CertificatePassword)
	if err != nil || cert == nil {
		return nil, fmt.Errorf("failed to decode pkcs12 certificate while creating spt: %v", err)
	}

	// Create the azcore.TokenCredential object
	certs := []*x509.Certificate{cert}
	opts := &azidentity.ClientCertificateCredentialOptions{
		ClientOptions: azcore.ClientOptions{
			Cloud: cloud.Configuration{
				ActiveDirectoryAuthorityHost: c.AADEndpoint,
			},
		},
	}
	return azidentity.NewClientCertificateCredential(c.TenantID, c.ClientID, certs, key, opts)
}

// Decode a certificate, either as a PKCS#12 (PFX) bundle, or as a single file with both certificate and key encoded in PEM blocks.
// The password is only used for PFX (and could be empty).
func (c CertConfig) decodeCertificate(data []byte, password string) (certificate *x509.Certificate, privateKey *rsa.PrivateKey, err error) {
	// First, try to decode the certificate as PKCS#12
	certificate, privateKey, err = c.decodePkcs12(data, password)
	if err == nil && certificate != nil {
		return certificate, privateKey, nil
	}

	// If it failed, try decoding as PEM
	certificate, privateKey, err = c.decodePEM(data)
	if err == nil && certificate != nil {
		return certificate, privateKey, nil
	}

	return nil, nil, errors.New("certificate is not valid")
}

func (c CertConfig) decodePkcs12(pkcs []byte, password string) (*x509.Certificate, *rsa.PrivateKey, error) {
	privateKey, certificate, err := pkcs12.Decode(pkcs, password)
	if err != nil {
		return nil, nil, err
	}

	rsaPrivateKey, isRsaKey := privateKey.(*rsa.PrivateKey)
	if !isRsaKey {
		return nil, nil, fmt.Errorf("PKCS#12 certificate must contain an RSA private key")
	}

	return certificate, rsaPrivateKey, nil
}

func (c CertConfig) decodePEM(data []byte) (certificate *x509.Certificate, privateKey *rsa.PrivateKey, err error) {
	// We should have 2 PEM blocks: a certificate and a key
	var (
		block     *pem.Block
		parsedKey any
		ok        bool
	)
	for i := 0; i < 2; i++ {
		block, data = pem.Decode(data)
		if block == nil {
			break
		}

		switch block.Type {
		case "CERTIFICATE":
			// If we already have a certificate decoded, return an error
			if certificate != nil {
				return nil, nil, errors.New("invalid certificate")
			}
			certificate, err = x509.ParseCertificate(block.Bytes)
			if err != nil {
				return nil, nil, err
			}
		case "PRIVATE KEY": // PKCS#8
			// If we already have a key decoded, return an error
			if privateKey != nil {
				return nil, nil, errors.New("invalid certificate")
			}
			parsedKey, err = x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, nil, err
			}
			privateKey, ok = parsedKey.(*rsa.PrivateKey)
			if !ok || privateKey == nil {
				return nil, nil, fmt.Errorf("certificate must contain an RSA private key")
			}
		case "RSA PRIVATE KEY": // PKCS#1
			// If we already have a key decoded, return an error
			if privateKey != nil {
				return nil, nil, errors.New("invalid certificate")
			}
			parsedKey, err = x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, nil, err
			}
			privateKey, ok = parsedKey.(*rsa.PrivateKey)
			if !ok || privateKey == nil {
				return nil, nil, fmt.Errorf("certificate must contain an RSA private key")
			}
		}
	}

	// We should have both a private key and a certificate
	if privateKey == nil || certificate == nil {
		return nil, nil, errors.New("invalid certificate")
	}
	return certificate, privateKey, nil
}

// MSIConfig provides the options to get a bearer authorizer through MSI.
type MSIConfig struct {
	ClientID string
}

// NewMSIConfig creates an MSIConfig object configured to obtain an Authorizer through MSI.
func NewMSIConfig() MSIConfig {
	return MSIConfig{}
}

// GetTokenCredential returns the azcore.TokenCredential object from MSI.
func (c MSIConfig) GetTokenCredential() (token azcore.TokenCredential, err error) {
	opts := &azidentity.ManagedIdentityCredentialOptions{}
	if c.ClientID != "" {
		opts.ID = azidentity.ClientID(c.ClientID)
	}
	return azidentity.NewManagedIdentityCredential(opts)
}

// GetAzureEnvironment returns the Azure environment for a given name, supporting aliases too.
func (s EnvironmentSettings) GetEnvironment(key string) (val string, ok bool) {
	return metadata.GetMetadataProperty(s.Metadata, MetadataKeys[key]...)
}
