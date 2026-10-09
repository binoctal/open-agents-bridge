package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/binoctal/open-agents-bridge/internal/crypto"
	"github.com/binoctal/open-agents-bridge/internal/machinekey"
	"github.com/binoctal/open-agents-bridge/internal/fingerprint"
	"github.com/spf13/cobra"
)

var (
	pairServerURL   string
	pairAutoStart   bool
	pairDevMode     bool
	pairStagingMode bool
	pairDevEmail    string
	pairDevPassword string
	pairMachineName  string
)

// Default server URLs
const (
	defaultAPIURL = "https://api.openagents.top"
	defaultWebURL = "https://openagents.top"
	stagingAPIURL = "https://api-staging.openagents.top"
	stagingWebURL = "https://openagents.top"
)

var pairCmd = &cobra.Command{
	Use:   "pair",
	Short: "Pair this machine with your Open Agents account",
	Long: `Pair this machine with your Open Agents account using a pairing code.

1. Go to the dashboard at https://openagents.top/dashboard/machines
2. Click "Add Machine" to get a pairing code
3. Enter the code when prompted

Examples:
  # Use default production server
  open-agents-bridge pair

  # Local development
  open-agents-bridge pair --server http://localhost:8787

  # Dev mode: auto-create test user and machine (localhost only)
  open-agents-bridge pair --dev --server http://localhost:8787`,
	Run: func(cmd *cobra.Command, args []string) {
		// --staging and --server are mutually exclusive
		if pairStagingMode && pairServerURL != "" {
			fmt.Fprintln(os.Stderr, "Error: --staging and --server cannot be used together")
			os.Exit(1)
		}

		// Use staging URL if --staging flag is set
		if pairStagingMode {
			pairServerURL = stagingAPIURL
		}

		// Use default production API URL if not specified
		if pairServerURL == "" {
			pairServerURL = defaultAPIURL
		}

		// Handle --dev mode
		if pairDevMode {
			runDevPair(cmd, args)
			return
		}

		reader := bufio.NewReader(os.Stdin)

		fmt.Println("Open Agents Machine Pairing")
		fmt.Println("==========================")
		fmt.Println()
		fmt.Printf("Using API server: %s\n", pairServerURL)

		// Determine dashboard URL based on server
		var dashboardURL string
		if pairServerURL == defaultAPIURL {
			dashboardURL = defaultWebURL + "/dashboard/machines"
		} else if pairServerURL == stagingAPIURL {
			dashboardURL = stagingWebURL + "/dashboard/machines"
		} else if config.IsLoopbackURL(pairServerURL) {
			dashboardURL = "http://localhost:5173/dashboard/machines"
		} else {
			dashboardURL = strings.TrimSuffix(pairServerURL, "/") + "/dashboard/machines"
		}

		fmt.Printf("1. Go to %s\n", dashboardURL)
		fmt.Println("2. Click 'Add Machine' to get a pairing code")
		fmt.Println()
		fmt.Print("Enter pairing code: ")

		code, _ := reader.ReadString('\n')
		code = strings.TrimSpace(code)

		if len(code) != 6 {
			fmt.Println("Error: Pairing code must be 6 characters")
			os.Exit(1)
		}

		fmt.Println("Generating encryption keys...")

		// Generate E2EE key pair
		keyPair, err := crypto.GenerateKeyPair()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error generating keys: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("Pairing...")

		// Call pairing API
		cfg, err := pairMachine(code, keyPair)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Pairing failed: %v\n", err)
			os.Exit(1)
		}

		// Use server-provided URL if available, otherwise compute from API URL
		if cfg.ServerURL == "" {
			wsURL := strings.Replace(pairServerURL, "http://", "ws://", 1)
			wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
			cfg.ServerURL = wsURL
		}

		// Save config - use server-provided name when --name is not set
		saveName := pairMachineName
		if saveName == "" && cfg.MachineName != "" {
			saveName = cfg.MachineName
		}

		if saveName != "" {
			cfg.MachineName = saveName
			if err := config.SaveMachine(saveName, cfg); err != nil {
				fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
				os.Exit(1)
			}
			// Switch to the newly paired machine

		} else {
			if err := config.Save(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
				os.Exit(1)
			}
		}

		fmt.Println()
		fmt.Println("✓ Machine paired successfully!")
		displayName := saveName
		if displayName == "" {
			displayName = cfg.MachineID
		}
		fmt.Printf("  Machine Name: %s\n", displayName)
		fmt.Printf("  Machine ID: %s\n", cfg.MachineID)
		fmt.Printf("  Server: %s\n", cfg.ServerURL)
		fmt.Println("  E2EE: Enabled")
		fmt.Println()

		// Auto-start if requested
		if pairAutoStart {
			fmt.Println("Starting bridge automatically...")
			fmt.Println()
			machineName = displayName
			startCmd.Run(cmd, args)
		} else {
			fmt.Printf("Run 'open-agents-bridge start -d %s' to start the bridge.\n", displayName)
		}
	},
}

// runDevPair handles --dev mode for quick local development setup
func runDevPair(cmd *cobra.Command, args []string) {
	// Safety check: only allow dev mode with localhost
	if !config.IsLoopbackURL(pairServerURL) {
		fmt.Fprintln(os.Stderr, "Error: --dev mode is only allowed with localhost servers")
		fmt.Fprintln(os.Stderr, "Use: open-agents-bridge pair --dev --server http://localhost:8787")
		os.Exit(1)
	}

	fmt.Println("Open Agents Dev Setup")
	fmt.Println("=====================")
	fmt.Println()
	fmt.Printf("Using API server: %s\n", pairServerURL)

	// Set defaults
	email := pairDevEmail
	if email == "" {
		email = "dev@openagents.local"
	}
	password := pairDevPassword
	if password == "" {
		password = "dev123456"
	}

	fmt.Printf("Setting up machine for: %s\n", email)

	// Call dev setup API
	cfg, err := devSetup(email, password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Dev setup failed: %v\n", err)
		os.Exit(1)
	}

	// Convert http(s) to ws(s)
	wsURL := strings.Replace(pairServerURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	cfg.ServerURL = wsURL

	// Generate encryption keys
	fmt.Println("Generating encryption keys...")
	keyPair, err := crypto.GenerateKeyPair()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating keys: %v\n", err)
		os.Exit(1)
	}
	cfg.PublicKey = keyPair.PublicKeyBase64()
	cfg.PrivateKey = base64.StdEncoding.EncodeToString(keyPair.PrivateKey[:])

	// Save config
	if pairMachineName != "" {
		cfg.MachineName = pairMachineName
		if err := config.SaveMachine(pairMachineName, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := config.Save(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println()
	fmt.Println("✓ Dev environment ready!")
	fmt.Printf("  User: %s\n", email)
	if pairMachineName != "" {
		fmt.Printf("  Machine Name: %s\n", pairMachineName)
	}
	fmt.Printf("  Machine ID: %s\n", cfg.MachineID)
	fmt.Printf("  Server: %s\n", cfg.ServerURL)
	fmt.Println()

	// Auto-start if requested
	if pairAutoStart {
		fmt.Println("Starting bridge automatically...")
		fmt.Println()
		startCmd.Run(cmd, args)
	} else {
		devName := pairMachineName
		if devName == "" {
			devName = "default"
		}
		fmt.Printf("Run 'open-agents-bridge start -d %s' to start the bridge.\n", devName)
	}
}

func init() {
	pairCmd.Flags().StringVarP(&pairServerURL, "server", "s", "", "API server URL (default: production server)")
	pairCmd.Flags().BoolVarP(&pairAutoStart, "auto-start", "a", false, "Automatically start bridge after pairing")
	pairCmd.Flags().BoolVarP(&pairStagingMode, "staging", "S", false, "Use staging server (api-staging.openagents.top)")
	pairCmd.Flags().BoolVarP(&pairDevMode, "dev", "d", false, "Development mode: auto-create test user and machine (localhost only)")
	pairCmd.Flags().StringVar(&pairDevEmail, "email", "", "Dev mode: custom email (default: dev@openagents.local)")
	pairCmd.Flags().StringVar(&pairDevPassword, "password", "", "Dev mode: custom password (default: dev123456)")
	pairCmd.Flags().StringVarP(&pairMachineName, "name", "n", "", "Machine name (for multi-machine support)")
}

type PairResponse struct {
	Success     bool   `json:"success"`
	UserID      string `json:"userId"`
	MachineID    string `json:"machineId"`
	MachineName  string `json:"machineName"`
	MachineToken string `json:"machineToken"`
	ServerURL   string `json:"serverUrl"`
	WebPubKey   string `json:"webPubKey,omitempty"`
	// DuplicateFingerprint is the server's hint that this machine looks already
	// paired (machine-model-ia D3). Informational only.
	DuplicateFingerprint bool           `json:"duplicate_fingerprint,omitempty"`
	Error                *ErrorResponse `json:"error,omitempty"`
}

type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func pairMachine(code string, keyPair *crypto.KeyPair) (*config.Config, error) {
	apiURL := strings.TrimSuffix(pairServerURL, "/") + "/api/machines/pair/verify"

	// 4b.1: the machine-bound signing key. Generated fresh per pairing so a
	// re-pair also rotates the binding; the private half never leaves this
	// host, the public half is bound server-side with the new token.
	machinePrivB64, machinePubB64, err := machinekey.Generate()
	if err != nil {
		return nil, fmt.Errorf("machine key: %v", err)
	}

	body := map[string]string{
		"pairCode":        code,
		"fingerprint":     fingerprint.Compute(),
		"machinePublicKey": machinePubB64,
	}
	bodyJSON, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:5173")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to server: %v", err)
	}
	defer resp.Body.Close()

	var result PairResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %v", err)
	}

	if !result.Success && result.Error != nil {
		return nil, fmt.Errorf("%s: %s", result.Error.Code, result.Error.Message)
	}

	if !result.Success {
		return nil, fmt.Errorf("pairing failed")
	}

	if result.DuplicateFingerprint {
		fmt.Println("Note: this computer looks like it was already paired to your account. You can remove the old entry in the web app.")
	}

	return &config.Config{
		UserID:           result.UserID,
		MachineID:        result.MachineID,
		MachineName:      result.MachineName,
		MachineToken:     result.MachineToken,
		ServerURL:        result.ServerURL,
		PublicKey:        keyPair.PublicKeyBase64(),
		PrivateKey:       base64.StdEncoding.EncodeToString(keyPair.PrivateKey[:]),
		MachinePrivateKey: machinePrivB64,
		WebPubKey:        result.WebPubKey,
	}, nil
}

// DevSetupResponse represents the response from /api/dev/setup
type DevSetupResponse struct {
	Success bool `json:"success"`
	User    struct {
		ID       string `json:"id"`
		Email    string `json:"email"`
		Password string `json:"password"`
	} `json:"user"`
	Machine struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Token string `json:"token"`
	} `json:"machine"`
	Config struct {
		UserID      string `json:"userId"`
		MachineID    string `json:"machineId"`
		MachineToken string `json:"machineToken"`
		ServerURL   string `json:"serverUrl"`
	} `json:"config"`
	Error *ErrorResponse `json:"error,omitempty"`
}

// devSetup calls the /api/dev/setup endpoint for quick local development
func devSetup(email, password string) (*config.Config, error) {
	apiURL := strings.TrimSuffix(pairServerURL, "/") + "/api/dev/setup"

	body := map[string]string{
		"email":    email,
		"password": password,
	}
	bodyJSON, _ := json.Marshal(body)

	// Create request with Origin header for CSRF bypass (dev mode only)
	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:5173")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to server: %v", err)
	}
	defer resp.Body.Close()

	var result DevSetupResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %v", err)
	}

	if !result.Success && result.Error != nil {
		return nil, fmt.Errorf("%s: %s", result.Error.Code, result.Error.Message)
	}

	if !result.Success {
		return nil, fmt.Errorf("dev setup failed")
	}

	return &config.Config{
		UserID:      result.Config.UserID,
		MachineID:    result.Config.MachineID,
		MachineToken: result.Config.MachineToken,
	}, nil
}
