package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lmittmann/tint"
	"github.com/spf13/viper"
)

var (
	debug = flag.Bool("debug", false, "Debug mode")
	//login = flag.Bool("login", false, "Login to Azure")
	//list   = flag.Bool("list", false, "List subscriptions")
	subs   = flag.String("subs", "", "Comma separated subscription names for PIM activation (required)")
	tenant = flag.String("tenant", "", "Azure Tenant ID")
)

func readConfig() {
	viper.SetConfigName("pim")  // name of config file (without extension)
	viper.SetConfigType("toml") // REQUIRED if the config file does not have the extension in the name
	viper.AddConfigPath(".")    // optionally look for config in the working directory
	err := viper.ReadInConfig() // Find and read the config file
	if err != nil {             // Handle errors reading the config file
		slog.Error("config file issue", "error", err)
		os.Exit(1)
	}
}

func contains(slice []string, item string) bool {
	for _, a := range slice {
		if a == item {
			return true
		}
	}
	return false
}

type CommandError struct {
	Err    error
	Stderr string
}

func (e *CommandError) LogError() {
	//return fmt.Sprintf("command failed: %v\nstderr: %s", e.Err, e.Stderr)
	slog.Error("command failed:", "error", e.Err, "stderr", e.Stderr)
}

func runCommand(cmd string, env []string) (string, *CommandError) {
	slog.Debug("Running command:", "cmd", cmd)

	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command("powershell", "-nologo", "-noprofile", cmd)
	} else {
		command = exec.Command("sh", "-c", cmd)
	}

	command.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &out
	command.Stderr = &stderr
	err := command.Run()
	if err != nil {
		cmdErr := &CommandError{
			Err:    err,
			Stderr: stderr.String(),
		}
		return "", cmdErr
	} else {
		slog.Debug("Successfully ran command")
		return out.String(), nil
	}
}

func azureCLIVersion() (string, *CommandError) {
	escape_quote := `\"` // escape double quotes for bash

	if runtime.GOOS == "windows" {
		escape_quote = "`\"" // escape double quotes for PowerShell
	}

	version, cmdErr := runCommand("az version --output json --query '" + escape_quote + "azure-cli" + escape_quote + "'", nil)

	if cmdErr != nil {
		return "", cmdErr
	} else {
		// Clean up version
		version = strings.Trim(strings.TrimSpace(version), "\"")
		return version, nil
	}
}

func azureCLIInstalled() bool {
	// check if Azure CLI is installed
	version, cmdErr := azureCLIVersion()

	if cmdErr != nil {
		cmdErr.LogError()
		slog.Error("Azure CLI not installed or not in path")
		slog.Info("Install from https://learn.microsoft.com/en-us/cli/azure/install-azure-cli")
		os.Exit(1)
	}

	slog.Debug("Azure CLI is installed", "version", version)
	return true
}

func azureLoggedIn() bool {
	_, cmdErr := runCommand("az account show", nil)

	if cmdErr != nil {
		// We don't need to log the error here unless the command failed for some other reason
		// Check the error message to see if it's a login error
		if strings.Contains(cmdErr.Stderr, "Please run 'az login' to setup account") {
			return false
		} else {
			cmdErr.LogError()
			slog.Error("Failed to check login status")
			os.Exit(1)
		}
	}
	return true
}

func getAccessToken() (string, error) {
	token, cmdErr := runCommand("az account get-access-token --query accessToken --output tsv", nil)

	if cmdErr != nil {
		cmdErr.LogError()
		return "", fmt.Errorf("Failed to get access token: %w", cmdErr.Err)
	}
	return token, nil
}

func loginToAzure(tenant string) error {
	if runtime.GOOS == "windows" {
		// Open the login URL in the default web browser vs using WAM (Windows Authentication Manager)
		// WAM behavior is still rather glitchy
		// TODO: Is there a way to just set this for the current session/process?
		_, cmdErr := runCommand("az config set core.enable_broker_on_windows=false", nil)

		if cmdErr != nil {
			cmdErr.LogError()
			slog.Error("failed to set Azure CLI config to disable WAM login")
			os.Exit(1)
		}
	}

	_, cmdErr := runCommand(fmt.Sprintf("az login --allow-no-subscriptions -t '%s'", tenant), nil)

	if cmdErr != nil {
		cmdErr.LogError()
		slog.Error("failed to login to Azure")
		os.Exit(1)
	}

	slog.Info("A web browser has been opened at https://login.microsoftonline.com. Please continue the login in the web browser. If no web browser is available or if the web browser fails to open, use device code flow with `az login --use-device-code`.")
	return nil
}

func prepareRequest(method, url, token string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, nil)

	if err != nil {
		return nil, fmt.Errorf("Failed to create request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", strings.TrimSpace(token)))
	return req, nil
}

func getRESIs(subscriptions []string, token string, client http.Client) (map[string]map[string]string, error) {
	req, err := prepareRequest("GET", "https://management.azure.com/providers/Microsoft.Authorization/roleEligibilityScheduleInstances?api-version=2020-10-01&$filter=asTarget()", token)

	resp, err := client.Do(req)
	if err != nil {
		slog.Error("Failed to get roleEligibilityScheduleInstances", "error", err, "StatusCode", resp.StatusCode)
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("Failed to read response body", "error", err)
		return nil, err
	}

	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		slog.Error("Failed to parse JSON response", "error", err)
		return nil, err
	}

	slog.Info("Successfully retrieved roleEligibilityScheduleInstances")
	slog.Debug("Data", "data", data)

	// make a map of maps
	roles := make(map[string]map[string]string)

	for _, value := range data["value"].([]interface{}) {
		properties := value.(map[string]interface{})["properties"].(map[string]interface{})
		scope := properties["expandedProperties"].(map[string]interface{})["scope"].(map[string]interface{})
		id := scope["id"].(string)
		sub := scope["displayName"].(string)
		slog.Info("Role found", "id", id, "subscription", sub)

		if contains(subscriptions, sub) {
			roles[sub] = make(map[string]string)
			slog.Info("Found roleEligibilityScheduleInstance", "subscription", sub)

			roles[sub]["id"] = id
			roles[sub]["displayName"] = sub
			roles[sub]["myPrincipalID"] = properties["principalId"].(string)
			roles[sub]["ownerRoleID"] = properties["roleDefinitionId"].(string)
			roles[sub]["roleEligibilityScheduleID"] = properties["roleEligibilityScheduleId"].(string)
			roles[sub]["subscription"] = strings.Split(id, "/")[2]
		}
	}

	return roles, err
}

func main() {
	flag.Parse()

	// Set the default log level
	lvl := new(slog.LevelVar)
	lvl.Set(slog.LevelInfo)

	if *debug {
		lvl.Set(slog.LevelDebug)
	}

	w := os.Stderr

	// create a new logger
	logger := slog.New(
		tint.NewHandler(w, &tint.Options{
			Level:      lvl,
			TimeFormat: time.Kitchen,
		}),
	)
	slog.SetDefault(logger)

	// Make sure subscriptions are provided
	if *subs == "" {
		slog.Error("No subscriptions provided")
		flag.Usage()
		os.Exit(1)
	}

	// Check if Azure CLI is installed
	_ = azureCLIInstalled()

	// read the config file
	readConfig()

	// Get the tenant ID
	if *tenant != "" {
		slog.Debug("Tenant ID provided on the command line", "tenant", *tenant)
	} else {
		*tenant = viper.GetString("tenant")
		if *tenant == "" {
			slog.Error("Tenant ID not found in config file")
			os.Exit(1)
		}
	}

	if azureLoggedIn() {
		slog.Info("Already logged in to Azure. Skipping login step.")
	} else {
		slog.Info("Not logged in to Azure. Logging in.")
		_ = loginToAzure(*tenant)
	}

	// Get the access token
	token, err := getAccessToken()
	if err != nil {
		slog.Error("Failed to get access token", "error", err)
		os.Exit(1)
	}

	// Create an HTTP client with a 30s timeout
	client := &http.Client{Timeout: 30 * time.Second}

	// Download the role eligibility schedule instances
	subscriptions := strings.Split(*subs, ",")
	roles, err := getRESIs(subscriptions, token, *client)

	if err != nil {
		slog.Error("Failed to get role eligibility schedule instances", "error", err)
	}

	var wg sync.WaitGroup

	for _, role := range roles {
		wg.Add(1)
		myPrincipalID := role["myPrincipalID"]
		ownerRoleID := role["ownerRoleID"]
		roleEligibilityScheduleID := role["roleEligibilityScheduleID"]
		sub := role["subscription"]
		displayName := role["displayName"]

		go func(myPrincipalID, ownerRoleID, roleEligibilityScheduleID, sub, token string, client http.Client) {
			defer wg.Done()

			pimBody := map[string]interface{}{
				"properties": map[string]interface{}{
					"principalId":                     myPrincipalID,
					"roleDefinitionId":                ownerRoleID,
					"requestType":                     "SelfActivate",
					"linkedRoleEligibilityScheduleId": roleEligibilityScheduleID,
					"justification":                   "AutoPIM Test",
					"scheduleInfo": map[string]interface{}{
						"expiration": map[string]interface{}{
							"type":        "AfterDuration",
							"endDateTime": nil,
							"duration":    "PT8H",
						},
					},
				},
			}

			slog.Info("Activating PIM", "displayName", displayName)
			uuidStr := uuid.New().String()
			url := fmt.Sprintf("https://management.azure.com/providers/Microsoft.Subscription/subscriptions/%s/providers/Microsoft.Authorization/roleAssignmentScheduleRequests/%s?api-version=2020-10-01", sub, uuidStr)

			pimBodyJSON, err := json.Marshal(pimBody)
			if err != nil {
				slog.Error("Failed to marshal JSON", "err", err)
			}

			req, err := http.NewRequest("PUT", url, bytes.NewBuffer(pimBodyJSON))
			if err != nil {
				slog.Error("Failed to create request", "err", err)
			}
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", strings.TrimSpace(token)))
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				slog.Error("Failed to activate PIM", "err", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != 200 && resp.StatusCode != 201 && resp.StatusCode != 202 {
				// Properly parse body
				body, err := io.ReadAll(resp.Body)

				if err != nil {
					slog.Error("Failed to read response body", "err", err)
					slog.Error("Unknown PIM activation state", "StatusCode", resp.StatusCode)
					return
				}

				// Parse the error message from the response
				var data map[string]interface{}
				if err := json.Unmarshal(body, &data); err != nil {
					slog.Error("Failed to parse JSON response", "err", err)
				}

				// Check if "error" key exists and is a map
				if errMap, ok := data["error"].(map[string]interface{}); ok {
					// Check if "code" key exists and is a string
					if code, ok := errMap["code"].(string); ok && code == "RoleAssignmentExists" {
						slog.Warn("PIM is already activated for", "displayName", displayName)
						return
					}
				}

				slog.Error("Failed to activate PIM", "StatusCode", resp.StatusCode, "body", string(body))
			}

			slog.Info("Successfully activated PIM")
		}(myPrincipalID, ownerRoleID, roleEligibilityScheduleID, sub, token, *client)
	}

	wg.Wait()
}
