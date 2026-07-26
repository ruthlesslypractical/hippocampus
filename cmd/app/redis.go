// Copyright (c) 2026 Ruthlessly Practical LLC. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license
// that can be found in the LICENSE file.

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ruthlesslypractical/hippocampus/internal/wordlist"
)

func (a *App) startLocalRedis() error {
	// Check if already running on target port
	// Always use 16379 for bundled Redis to avoid collisions with system redis
	port := "16379"
	if a.dialLocalRedis(port) {
		// Already running — just connect
		a.config.RedisHost = "127.0.0.1"
		a.config.RedisPort = port
		return nil
	}

	// Find redis-server: system first, bundled fallback
	redisPath := ""
	if sysPath, err := exec.LookPath("redis-server"); err == nil {
		redisPath = sysPath
	} else {
		redisPath = a.bundledBinaryPath("redis-server")
		if _, err := os.Stat(redisPath); err != nil {
			return fmt.Errorf("redis-server not found (install Redis or rebuild the app bundle)")
		}
	}

	// Ensure data directory exists
	dataDir := a.dataDir()
	os.MkdirAll(dataDir, 0o755)

	// Generate redis.conf (don't clobber existing)
	confPath := filepath.Join(a.appSupportDir(), "redis.conf")
	if _, err := os.Stat(confPath); err != nil {
		// First run — generate config with random password
		password := generatePassword()
		a.config.RedisPassword = password
		a.config.RedisHost = "127.0.0.1"
		a.config.RedisPort = port
		a.saveConfig()

		// Find redisearch module path
		rsModule := a.bundledBinaryPath("redisearch.so")
		moduleLine := ""
		if _, err := os.Stat(rsModule); err == nil {
			moduleLine = fmt.Sprintf("loadmodule \"%s\"\n", rsModule)
		}

		conf := fmt.Sprintf(`# Hippocampus Redis Configuration
# Generated on first run. Edit freely — will not be overwritten.

bind %s
port %s
requirepass %s

# Persistence
dir "%s"
dbfilename dump.rdb
appendonly yes
appendfsync everysec

# Memory
maxmemory 256mb
maxmemory-policy noeviction

# Modules
%s
# Logging
loglevel notice
logfile "%s/redis.log"
`, a.redisBind(), port, password, dataDir, moduleLine, filepath.Join(a.logsDir()))

		// If network mode, add TLS configuration
		if a.config.NetworkMode && a.config.TLSCertPath != "" {
			tlsConf := fmt.Sprintf(`
# TLS (network mode — required for LAN access)
tls-port %s
port 0
tls-cert-file "%s"
tls-key-file "%s"
tls-auth-clients no
`, port, a.config.TLSCertPath, a.config.TLSKeyPath)
			conf += tlsConf
		}

		os.MkdirAll(filepath.Dir(confPath), 0o755)
		os.MkdirAll(a.logsDir(), 0o755)
		if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
			return fmt.Errorf("writing redis.conf: %w", err)
		}
	} else {
		// Config exists — read password from the redis.conf file
		if confData, err := os.ReadFile(confPath); err == nil {
			for _, line := range strings.Split(string(confData), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "requirepass ") {
					a.config.RedisPassword = strings.TrimPrefix(line, "requirepass ")
					break
				}
			}
		}
		if a.config.RedisPort == "" {
			a.config.RedisPort = port
		}
		a.config.RedisHost = "127.0.0.1"
		a.config.RedisPort = port
		a.saveConfig()
	}

	// Start redis-server with the config
	cmd := exec.Command(redisPath, confPath)
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting redis-server: %w", err)
	}
	a.redisCmd = cmd

	// Wait for it to accept connections
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		if a.dialLocalRedis(port) {
			return nil
		}
	}
	return fmt.Errorf("redis-server started but not accepting connections")
}

// dialLocalRedis checks if local Redis is accepting connections.
// Uses TLS dial when in network mode (since plain port is disabled).
func (a *App) dialLocalRedis(port string) bool {
	addr := "127.0.0.1:" + port
	if a.config.NetworkMode {
		conn, err := tls.DialWithDialer(
			&net.Dialer{Timeout: time.Second},
			"tcp", addr,
			&tls.Config{InsecureSkipVerify: true},
		)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func generatePassword() string {
	// Generate a 6-word passphrase — easy to read aloud, 72 bits of entropy.
	// 4096 words × 6 = 12 bits/word × 6 = 72 bits (equivalent to 12-char random).
	words := wordlist.Words()
	b := make([]byte, 12)
	f, err := os.Open("/dev/urandom")
	if err != nil {
		t := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(t >> (i * 4))
		}
	} else {
		f.Read(b)
		f.Close()
	}
	selected := make([]string, 6)
	for i := 0; i < 6; i++ {
		idx := (int(b[i*2])<<8 | int(b[i*2+1])) % len(words)
		selected[i] = words[idx]
	}
	return strings.Join(selected, "-")
}

// redisBind returns the bind address based on network mode.
func (a *App) redisBind() string {
	if a.config.NetworkMode {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// regenerateRedisConf writes a new redis.conf reflecting current config state.
// Handles bind address (localhost vs 0.0.0.0) and TLS on/off.
func (a *App) regenerateRedisConf() error {
	confPath := filepath.Join(a.appSupportDir(), "redis.conf")
	port := a.config.RedisPort
	if port == "" {
		port = "16379"
	}
	password := a.localRedisPassword()
	dataDir := a.dataDir()

	rsModule := a.bundledBinaryPath("redisearch.so")
	moduleLine := ""
	if _, err := os.Stat(rsModule); err == nil {
		moduleLine = fmt.Sprintf("loadmodule \"%s\"\n", rsModule)
	}

	var conf strings.Builder
	conf.WriteString("# Hippocampus Redis Configuration (auto-regenerated)\n\n")

	if a.config.NetworkMode && a.config.TLSCertPath != "" {
		conf.WriteString(fmt.Sprintf("bind %s\n", a.redisBind()))
		conf.WriteString("port 0\n")
		conf.WriteString(fmt.Sprintf("tls-port %s\n", port))
		conf.WriteString(fmt.Sprintf("tls-cert-file \"%s\"\n", a.config.TLSCertPath))
		conf.WriteString(fmt.Sprintf("tls-key-file \"%s\"\n", a.config.TLSKeyPath))
		conf.WriteString(fmt.Sprintf("tls-ca-cert-file \"%s\"\n", a.config.TLSCertPath))
		conf.WriteString("tls-auth-clients no\n")
	} else {
		conf.WriteString(fmt.Sprintf("bind %s\n", a.redisBind()))
		conf.WriteString(fmt.Sprintf("port %s\n", port))
	}

	conf.WriteString(fmt.Sprintf("requirepass %s\n", password))
	conf.WriteString(fmt.Sprintf("\ndir \"%s\"\n", dataDir))
	conf.WriteString("dbfilename dump.rdb\nappendonly yes\nappendfsync everysec\n")
	conf.WriteString("\nmaxmemory 256mb\nmaxmemory-policy noeviction\n")
	conf.WriteString("\n" + moduleLine)
	conf.WriteString(fmt.Sprintf("\nloglevel notice\nlogfile \"%s/redis.log\"\n", a.logsDir()))

	os.MkdirAll(filepath.Dir(confPath), 0o755)
	os.MkdirAll(a.logsDir(), 0o755)
	return os.WriteFile(confPath, []byte(conf.String()), 0o600)
}

// restartLocalRedis stops the running Redis and starts it again with current config.
func (a *App) restartLocalRedis() error {
	if a.redisClient != nil {
		a.redisClient.Close()
		a.redisClient = nil
	}
	if a.redisCmd != nil && a.redisCmd.Process != nil {
		a.redisCmd.Process.Kill()
		a.redisCmd.Wait()
		a.redisCmd = nil
	}
	time.Sleep(300 * time.Millisecond)
	if err := a.startLocalRedis(); err != nil {
		return err
	}
	return a.Connect()
}

// localRedisPassword reads the password from redis.conf (source of truth for local mode).
// Falls back to a.config.RedisPassword if redis.conf doesn't exist.
func (a *App) localRedisPassword() string {
	confPath := filepath.Join(a.appSupportDir(), "redis.conf")
	if confData, err := os.ReadFile(confPath); err == nil {
		for _, line := range strings.Split(string(confData), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "requirepass ") {
				return strings.TrimPrefix(line, "requirepass ")
			}
		}
	}
	return a.config.RedisPassword
}

// GetRemoteFingerprint TLS-dials a remote Redis instance and returns its verify phrase.
func (a *App) GetRemoteFingerprint(host string, port string) (string, error) {
	if host == "" {
		return "", fmt.Errorf("host is required")
	}
	if port == "" {
		port = "16379"
	}

	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 5 * time.Second},
		"tcp", host+":"+port,
		&tls.Config{InsecureSkipVerify: true},
	)
	if err != nil {
		return "", fmt.Errorf("TLS dial failed: %w", err)
	}
	defer conn.Close()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", fmt.Errorf("no certificates presented by server")
	}

	fingerprint := sha256.Sum256(certs[0].Raw)
	return fingerprintToWords(fmt.Sprintf("%x", fingerprint)), nil
}

// EnableNetworkMode enables LAN-accessible Redis with forced TLS.
// Generates a self-signed cert if one doesn't exist.
func (a *App) EnableNetworkMode() error {
	// Generate TLS cert if needed
	if a.config.TLSCertPath == "" || a.config.TLSKeyPath == "" {
		if err := a.generateTLSCert(); err != nil {
			return fmt.Errorf("generating TLS certificate: %w", err)
		}
	}

	a.config.NetworkMode = true
	a.config.RedisTLS = true // Force TLS when network-exposed
	a.saveConfig()

	return nil
}

// DisableNetworkMode switches back to localhost-only binding.
func (a *App) DisableNetworkMode() {
	a.config.NetworkMode = false
	a.saveConfig()
}

// GetNetworkInfo returns connection details for other machines on the LAN.
func (a *App) GetNetworkInfo() map[string]string {
	port := a.config.RedisPort
	if port == "" {
		port = "16379"
	}

	info := map[string]string{
		"enabled":     fmt.Sprintf("%v", a.config.NetworkMode),
		"port":        port,
		"tls":         fmt.Sprintf("%v", a.config.RedisTLS),
		"fingerprint": a.config.TLSFingerprint,
		"password":    a.localRedisPassword(),
	}

	// Get local IP addresses for display
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		var ips []string
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
				ips = append(ips, ipNet.IP.String())
			}
		}
		info["local_ips"] = strings.Join(ips, ",")
	}

	return info
}

// generateTLSCert creates a self-signed ECDSA certificate for TLS.
// The cert is valid for all local IPs + localhost, good for 5 years.
func (a *App) generateTLSCert() error {
	certDir := filepath.Join(a.appSupportDir(), "tls")
	os.MkdirAll(certDir, 0o700)

	certPath := filepath.Join(certDir, "server.crt")
	keyPath := filepath.Join(certDir, "server.key")

	// Generate ECDSA private key
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating key: %w", err)
	}

	// Build certificate template
	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Hippocampus"},
			CommonName:   "Hippocampus Memory Server",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(5 * 365 * 24 * time.Hour), // 5 years
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	// Add all local IPs as SANs
	template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	template.DNSNames = []string{"localhost"}

	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
				template.IPAddresses = append(template.IPAddresses, ipNet.IP)
			}
		}
	}

	// Self-sign
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("creating certificate: %w", err)
	}

	// Write cert
	certFile, err := os.Create(certPath)
	if err != nil {
		return fmt.Errorf("writing cert: %w", err)
	}
	pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	certFile.Close()

	// Write key
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshaling key: %w", err)
	}
	keyFile, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("writing key: %w", err)
	}
	pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	keyFile.Close()

	// Compute fingerprint for client trust verification
	fingerprint := sha256.Sum256(certDER)
	fingerprintHex := fmt.Sprintf("%x", fingerprint)

	// Store paths and fingerprint (both hex and verbal)
	a.config.TLSCertPath = certPath
	a.config.TLSKeyPath = keyPath
	a.config.TLSFingerprint = fingerprintToWords(fingerprintHex)
	a.saveConfig()

	return nil
}

// fingerprintToWords converts a hex fingerprint to a 6-word verbal hash.
// Designed to be read aloud over the phone: "harbor-crystal-posing-timber-frisk-ablaze"
func fingerprintToWords(hexFingerprint string) string {
	words := wordlist.Words()

	var selected [6]string
	for i := 0; i < 6; i++ {
		// 3 hex chars = 12 bits → mod into word list
		hexTriplet := hexFingerprint[i*3 : i*3+3]
		var val int
		fmt.Sscanf(hexTriplet, "%x", &val)
		selected[i] = words[val%len(words)]
	}

	return strings.Join(selected[:], "-")
}
