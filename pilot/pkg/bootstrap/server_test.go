// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package bootstrap

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"golang.org/x/net/context"
	"golang.org/x/net/http2"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/keycertbundle"
	"istio.io/istio/pilot/pkg/server"
	kubecontroller "istio.io/istio/pilot/pkg/serviceregistry/kube/controller"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/pkg/testcerts"
	"istio.io/pkg/filewatcher"
)

func loadCertFilesAtPaths(t TLSFSLoadPaths) error {
	// create cert directories if not existing
	if err := os.MkdirAll(filepath.Dir(t.testTLSCertFilePath), os.ModePerm); err != nil {
		return fmt.Errorf("Mkdirall(%v) failed: %v", t.testTLSCertFilePath, err)
	}

	if err := os.MkdirAll(filepath.Dir(t.testTLSKeyFilePath), os.ModePerm); err != nil {
		return fmt.Errorf("Mkdirall(%v) failed: %v", t.testTLSKeyFilePath, err)
	}

	if err := os.MkdirAll(filepath.Dir(t.testCaCertFilePath), os.ModePerm); err != nil {
		return fmt.Errorf("Mkdirall(%v) failed: %v", t.testCaCertFilePath, err)
	}

	// load key and cert files.
	if err := os.WriteFile(t.testTLSCertFilePath, testcerts.ServerCert, 0o644); err != nil { // nolint: vetshadow
		return fmt.Errorf("WriteFile(%v) failed: %v", t.testTLSCertFilePath, err)
	}
	if err := os.WriteFile(t.testTLSKeyFilePath, testcerts.ServerKey, 0o644); err != nil { // nolint: vetshadow
		return fmt.Errorf("WriteFile(%v) failed: %v", t.testTLSKeyFilePath, err)
	}
	if err := os.WriteFile(t.testCaCertFilePath, testcerts.CACert, 0o644); err != nil { // nolint: vetshadow
		return fmt.Errorf("WriteFile(%v) failed: %v", t.testCaCertFilePath, err)
	}

	return nil
}

func cleanupCertFileSystemFiles(t TLSFSLoadPaths) error {
	if err := os.Remove(t.testTLSCertFilePath); err != nil {
		return fmt.Errorf("Test cleanup failed, could not delete %s", t.testTLSCertFilePath)
	}

	if err := os.Remove(t.testTLSKeyFilePath); err != nil {
		return fmt.Errorf("Test cleanup failed, could not delete %s", t.testTLSKeyFilePath)
	}

	if err := os.Remove(t.testCaCertFilePath); err != nil {
		return fmt.Errorf("Test cleanup failed, could not delete %s", t.testCaCertFilePath)
	}
	return nil
}

// This struct will indicate for each test case
// where tls assets will be loaded on disk
type TLSFSLoadPaths struct {
	testTLSCertFilePath string
	testTLSKeyFilePath  string
	testCaCertFilePath  string
}

func TestNewServerCertInit(t *testing.T) {
	configDir := t.TempDir()

	tlsArgCertsDir := t.TempDir()

	tlsArgcertFile := filepath.Join(tlsArgCertsDir, "cert-file.pem")
	tlsArgkeyFile := filepath.Join(tlsArgCertsDir, "key-file.pem")
	tlsArgcaCertFile := filepath.Join(tlsArgCertsDir, "ca-cert.pem")

	cases := []struct {
		name                      string
		FSCertsPaths              TLSFSLoadPaths
		tlsOptions                *TLSOptions
		enableCA                  bool
		certProvider              string
		expNewCert                bool
		expCert                   []byte
		expKey                    []byte
		expSecureDiscoveryService bool
	}{
		{
			name:         "Load from existing DNS cert",
			FSCertsPaths: TLSFSLoadPaths{tlsArgcertFile, tlsArgkeyFile, tlsArgcaCertFile},
			tlsOptions: &TLSOptions{
				CertFile:   tlsArgcertFile,
				KeyFile:    tlsArgkeyFile,
				CaCertFile: tlsArgcaCertFile,
			},
			enableCA:                  false,
			certProvider:              constants.CertProviderKubernetes,
			expNewCert:                false,
			expCert:                   testcerts.ServerCert,
			expKey:                    testcerts.ServerKey,
			expSecureDiscoveryService: true,
		},
		{
			name:         "Create new DNS cert using Istiod",
			FSCertsPaths: TLSFSLoadPaths{},
			tlsOptions: &TLSOptions{
				CertFile:   "",
				KeyFile:    "",
				CaCertFile: "",
			},
			enableCA:                  true,
			certProvider:              constants.CertProviderIstiod,
			expNewCert:                true,
			expCert:                   []byte{},
			expKey:                    []byte{},
			expSecureDiscoveryService: true,
		},
		{
			name:         "No DNS cert created because CA is disabled",
			FSCertsPaths: TLSFSLoadPaths{},
			tlsOptions:   &TLSOptions{},
			enableCA:     false,
			certProvider: constants.CertProviderIstiod,
			expNewCert:   false,
			expCert:      []byte{},
			expKey:       []byte{},
		},
		{
			name: "DNS cert loaded because it is in known even if CA is Disabled",
			FSCertsPaths: TLSFSLoadPaths{
				constants.DefaultPilotTLSCert,
				constants.DefaultPilotTLSKey,
				constants.DefaultPilotTLSCaCert,
			},
			tlsOptions:                &TLSOptions{},
			enableCA:                  false,
			certProvider:              constants.CertProviderNone,
			expNewCert:                false,
			expCert:                   testcerts.ServerCert,
			expKey:                    testcerts.ServerKey,
			expSecureDiscoveryService: true,
		},
		{
			name: "DNS cert loaded from known location, even if CA is Disabled, with a fallback CA path",
			FSCertsPaths: TLSFSLoadPaths{
				constants.DefaultPilotTLSCert,
				constants.DefaultPilotTLSKey,
				constants.DefaultPilotTLSCaCertAlternatePath,
			},
			tlsOptions:                &TLSOptions{},
			enableCA:                  false,
			certProvider:              constants.CertProviderNone,
			expNewCert:                false,
			expCert:                   testcerts.ServerCert,
			expKey:                    testcerts.ServerKey,
			expSecureDiscoveryService: true,
		},
		{
			name:         "No cert provider",
			FSCertsPaths: TLSFSLoadPaths{},
			tlsOptions:   &TLSOptions{},
			enableCA:     true,
			certProvider: constants.CertProviderNone,
			expNewCert:   false,
			expCert:      []byte{},
			expKey:       []byte{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			test.SetForTest(t, &features.PilotCertProvider, c.certProvider)
			test.SetForTest(t, &features.EnableCAServer, c.enableCA)

			// check if we have some tls assets to write for test
			if c.FSCertsPaths != (TLSFSLoadPaths{}) {
				err := loadCertFilesAtPaths(c.FSCertsPaths)
				if err != nil {
					t.Fatal(err.Error())
				}

				defer cleanupCertFileSystemFiles(c.FSCertsPaths)
			}

			args := NewPilotArgs(func(p *PilotArgs) {
				p.Namespace = "istio-system"
				p.ServerOptions = DiscoveryServerOptions{
					// Dynamically assign all ports.
					HTTPAddr:       ":0",
					MonitoringAddr: ":0",
					GRPCAddr:       ":0",
					SecureGRPCAddr: ":0",
					TLSOptions:     *c.tlsOptions,
				}
				p.RegistryOptions = RegistryOptions{
					FileDir: configDir,
				}

				p.ShutdownDuration = 1 * time.Millisecond
			})
			g := NewWithT(t)
			s, err := NewServer(args, func(s *Server) {
				s.kubeClient = kube.NewFakeClient()
			})
			g.Expect(err).To(Succeed())
			stop := make(chan struct{})
			g.Expect(s.Start(stop)).To(Succeed())
			defer func() {
				close(stop)
				s.WaitUntilCompletion()
			}()

			if c.expNewCert {
				if istiodCert, err := s.getIstiodCertificate(nil); istiodCert == nil || err != nil {
					t.Errorf("Istiod failed to generate new DNS cert")
				}
			} else {
				if len(c.expCert) != 0 {
					if !checkCert(t, s, c.expCert, c.expKey) {
						t.Errorf("Istiod certifiate does not match the expectation")
					}
				} else {
					if _, err := s.getIstiodCertificate(nil); err == nil {
						t.Errorf("Istiod should not generate new DNS cert")
					}
				}
			}

			if c.expSecureDiscoveryService {
				if s.secureGrpcServer == nil {
					t.Errorf("Istiod secure grpc server was not started.")
				}
			}
		})
	}
}

func TestReloadIstiodCert(t *testing.T) {
	dir := t.TempDir()
	stop := make(chan struct{})
	s := &Server{
		fileWatcher:             filewatcher.NewWatcher(),
		server:                  server.New(),
		istiodCertBundleWatcher: keycertbundle.NewWatcher(),
	}

	defer func() {
		close(stop)
		_ = s.fileWatcher.Close()
	}()

	certFile := filepath.Join(dir, "cert-file.yaml")
	keyFile := filepath.Join(dir, "key-file.yaml")
	caFile := filepath.Join(dir, "ca-file.yaml")

	// load key and cert files.
	if err := os.WriteFile(certFile, testcerts.ServerCert, 0o644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", certFile, err)
	}
	if err := os.WriteFile(keyFile, testcerts.ServerKey, 0o644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", keyFile, err)
	}

	if err := os.WriteFile(caFile, testcerts.CACert, 0o644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", caFile, err)
	}

	tlsOptions := TLSOptions{
		CertFile:   certFile,
		KeyFile:    keyFile,
		CaCertFile: caFile,
	}

	// setup cert watches.
	if err := s.initCertificateWatches(tlsOptions); err != nil {
		t.Fatalf("initCertificateWatches failed: %v", err)
	}

	if err := s.initIstiodCertLoader(); err != nil {
		t.Fatalf("istiod unable to load its cert")
	}

	if err := s.server.Start(stop); err != nil {
		t.Fatalf("Could not invoke startFuncs: %v", err)
	}

	// Validate that the certs are loaded.
	if !checkCert(t, s, testcerts.ServerCert, testcerts.ServerKey) {
		t.Errorf("Istiod certifiate does not match the expectation")
	}

	// Update cert/key files.
	if err := os.WriteFile(tlsOptions.CertFile, testcerts.RotatedCert, 0o644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", tlsOptions.CertFile, err)
	}
	if err := os.WriteFile(tlsOptions.KeyFile, testcerts.RotatedKey, 0o644); err != nil { // nolint: vetshadow
		t.Fatalf("WriteFile(%v) failed: %v", tlsOptions.KeyFile, err)
	}

	g := NewWithT(t)

	// Validate that istiod cert is updated.
	g.Eventually(func() bool {
		return checkCert(t, s, testcerts.RotatedCert, testcerts.RotatedKey)
	}, "10s", "100ms").Should(BeTrue())
}

func TestNewServer(t *testing.T) {
	// All of the settings to apply and verify. Currently just testing domain suffix,
	// but we should expand this list.
	cases := []struct {
		name             string
		domain           string
		expectedDomain   string
		enableSecureGRPC bool
		jwtRule          string
	}{
		{
			name:           "default domain",
			domain:         "",
			expectedDomain: constants.DefaultClusterLocalDomain,
		},
		{
			name:           "default domain with JwtRule",
			domain:         "",
			expectedDomain: constants.DefaultClusterLocalDomain,
			jwtRule:        `{"issuer": "foo", "jwks_uri": "baz", "audiences": ["aud1", "aud2"]}`,
		},
		{
			name:           "override domain",
			domain:         "mydomain.com",
			expectedDomain: "mydomain.com",
		},
		{
			name:             "override default secured grpc port",
			domain:           "",
			expectedDomain:   constants.DefaultClusterLocalDomain,
			enableSecureGRPC: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			configDir := t.TempDir()

			var secureGRPCPort int
			var err error
			if c.enableSecureGRPC {
				secureGRPCPort, err = findFreePort()
				if err != nil {
					t.Errorf("unable to find a free port: %v", err)
					return
				}
			}

			args := NewPilotArgs(func(p *PilotArgs) {
				p.Namespace = "istio-system"
				p.ServerOptions = DiscoveryServerOptions{
					// Dynamically assign all ports.
					HTTPAddr:       ":0",
					MonitoringAddr: ":0",
					GRPCAddr:       ":0",
					SecureGRPCAddr: fmt.Sprintf(":%d", secureGRPCPort),
				}
				p.RegistryOptions = RegistryOptions{
					KubeOptions: kubecontroller.Options{
						DomainSuffix: c.domain,
					},
					FileDir: configDir,
				}

				p.ShutdownDuration = 1 * time.Millisecond

				p.JwtRule = c.jwtRule
			})

			g := NewWithT(t)
			s, err := NewServer(args, func(s *Server) {
				s.kubeClient = kube.NewFakeClient()
			})
			g.Expect(err).To(Succeed())
			stop := make(chan struct{})
			g.Expect(s.Start(stop)).To(Succeed())
			defer func() {
				close(stop)
				s.WaitUntilCompletion()
			}()

			g.Expect(s.environment.DomainSuffix).To(Equal(c.expectedDomain))

			if c.enableSecureGRPC {
				tcpAddr := s.secureGrpcAddress
				_, port, err := net.SplitHostPort(tcpAddr)
				if err != nil {
					t.Errorf("invalid SecureGrpcListener addr %v", err)
				}
				g.Expect(port).To(Equal(strconv.Itoa(secureGRPCPort)))
			}
		})
	}
}

func TestMultiplex(t *testing.T) {
	configDir := t.TempDir()

	var secureGRPCPort int
	var err error

	args := NewPilotArgs(func(p *PilotArgs) {
		p.Namespace = "istio-system"
		p.ServerOptions = DiscoveryServerOptions{
			// Dynamically assign all ports.
			HTTPAddr:       ":0",
			MonitoringAddr: ":0",
			GRPCAddr:       "",
			SecureGRPCAddr: fmt.Sprintf(":%d", secureGRPCPort),
		}
		p.RegistryOptions = RegistryOptions{
			FileDir: configDir,
		}

		p.ShutdownDuration = 1 * time.Millisecond
	})

	g := NewWithT(t)
	s, err := NewServer(args, func(s *Server) {
		s.kubeClient = kube.NewFakeClient()
	})
	g.Expect(err).To(Succeed())
	stop := make(chan struct{})
	g.Expect(s.Start(stop)).To(Succeed())
	defer func() {
		close(stop)
		s.WaitUntilCompletion()
	}()
	t.Run("h1", func(t *testing.T) {
		c := http.Client{}
		defer c.CloseIdleConnections()
		resp, err := c.Get("http://" + s.httpAddr + "/validate")
		assert.NoError(t, err)
		// Validate returns 400 on no body; if we got this the request works
		assert.Equal(t, resp.StatusCode, 400)
		resp.Body.Close()
	})
	t.Run("h2", func(t *testing.T) {
		c := http.Client{
			Transport: &http2.Transport{
				// Golang doesn't have first class support for h2c, so we provide some workarounds
				// See https://www.mailgun.com/blog/http-2-cleartext-h2c-client-example-go/
				// So http2.Transport doesn't complain the URL scheme isn't 'https'
				AllowHTTP: true,
				// Pretend we are dialing a TLS endpoint. (Note, we ignore the passed tls.Config)
				DialTLSContext: func(_ context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					return net.Dial(network, addr)
				},
			},
		}
		defer c.CloseIdleConnections()

		resp, err := c.Get("http://" + s.httpAddr + "/validate")
		assert.NoError(t, err)
		// Validate returns 400 on no body; if we got this the request works
		assert.Equal(t, resp.StatusCode, 400)
		resp.Body.Close()
	})
}

func TestIstiodCipherSuites(t *testing.T) {
	cases := []struct {
		name               string
		serverCipherSuites []uint16
		clientCipherSuites []uint16
		expectSuccess      bool
	}{
		{
			name:          "default cipher suites",
			expectSuccess: true,
		},
		{
			name:               "client and istiod cipher suites match",
			serverCipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
			clientCipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
			expectSuccess:      true,
		},
		{
			name:               "client and istiod cipher suites mismatch",
			serverCipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
			clientCipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384, tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384},
			expectSuccess:      false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			configDir := t.TempDir()

			port, err := findFreePort()
			if err != nil {
				t.Errorf("unable to find a free port: %v", err)
				return
			}

			args := NewPilotArgs(func(p *PilotArgs) {
				p.Namespace = "istio-system"
				p.ServerOptions = DiscoveryServerOptions{
					// Dynamically assign all ports.
					HTTPAddr:       ":0",
					MonitoringAddr: ":0",
					GRPCAddr:       ":0",
					HTTPSAddr:      fmt.Sprintf(":%d", port),
					TLSOptions: TLSOptions{
						CipherSuits: c.serverCipherSuites,
					},
				}
				p.RegistryOptions = RegistryOptions{
					KubeConfig: "config",
					FileDir:    configDir,
				}

				// Include all of the default plugins
				p.ShutdownDuration = 1 * time.Millisecond
			})

			g := NewWithT(t)
			s, err := NewServer(args, func(s *Server) {
				s.kubeClient = kube.NewFakeClient()
			})
			g.Expect(err).To(Succeed())

			stop := make(chan struct{})
			g.Expect(s.Start(stop)).To(Succeed())
			defer func() {
				close(stop)
				s.WaitUntilCompletion()
			}()

			// nolint: gosec // test only code
			httpsReadyClient := &http.Client{
				Timeout: time.Second,
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{
						InsecureSkipVerify: true,
						CipherSuites:       c.clientCipherSuites,
						MinVersion:         tls.VersionTLS12,
						MaxVersion:         tls.VersionTLS12,
					},
				},
			}

			retry.UntilSuccessOrFail(t, func() error {
				req := &http.Request{
					Method: http.MethodGet,
					URL: &url.URL{
						Scheme: "https",
						Host:   s.httpsServer.Addr,
						Path:   HTTPSHandlerReadyPath,
					},
				}
				response, err := httpsReadyClient.Do(req)
				if c.expectSuccess && err != nil {
					return fmt.Errorf("expect success but got err %v", err)
				}
				if !c.expectSuccess && err == nil {
					return fmt.Errorf("expect failure but succeeded")
				}
				if response != nil {
					response.Body.Close()
				}
				return nil
			})
		})
	}
}

func TestInitOIDC(t *testing.T) {
	tests := []struct {
		name      string
		expectErr bool
		jwtRule   string
	}{
		{
			name:      "valid jwt rule",
			expectErr: false,
			jwtRule:   `{"issuer": "foo", "jwks_uri": "baz", "audiences": ["aud1", "aud2"]}`,
		},
		{
			name:      "invalid jwt rule",
			expectErr: true,
			jwtRule:   "invalid",
		},
		{
			name:      "jwt rule with invalid audiences",
			expectErr: true,
			// audiences must be a string array
			jwtRule: `{"issuer": "foo", "jwks_uri": "baz", "audiences": "aud1"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := &PilotArgs{JwtRule: tt.jwtRule}

			_, err := initOIDC(args, "domain-foo")
			gotErr := err != nil
			if gotErr != tt.expectErr {
				t.Errorf("expect error is %v while actual error is %v", tt.expectErr, gotErr)
			}
		})
	}
}

func checkCert(t *testing.T, s *Server, cert, key []byte) bool {
	t.Helper()
	actual, err := s.getIstiodCertificate(nil)
	if err != nil {
		t.Fatalf("fail to load fetch certs.")
	}
	expected, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatalf("fail to load test certs.")
	}
	return bytes.Equal(actual.Certificate[0], expected.Certificate[0])
}

func findFreePort() (int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("invalid listen address: %q", ln.Addr().String())
	}
	return tcpAddr.Port, nil
}
