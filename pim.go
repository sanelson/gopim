package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lmittmann/tint"
	"github.com/spf13/viper"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity/cache"
)

// Lifted caching example from SDK docs
// see: https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azidentity#example-package-PersistentUserAuthentication
func retrieveRecord(authRecordPath string) (azidentity.AuthenticationRecord, error) {
	record := azidentity.AuthenticationRecord{}
	b, err := os.ReadFile(authRecordPath)
	if err == nil {
		err = json.Unmarshal(b, &record)
	}
	return record, err
}

func storeRecord(record azidentity.AuthenticationRecord, authRecordPath string) error {
	b, err := json.Marshal(record)
	if err == nil {
		err = os.WriteFile(authRecordPath, b, 0600)
	}
	return err
}

func readConfig(configDir string) {
	viper.SetConfigName("pim")     // name of config file (without extension)
	viper.SetConfigType("toml")    // REQUIRED if the config file does not have the extension in the name
	viper.AddConfigPath(configDir) // Main config location
	viper.AddConfigPath(".")       // optionally look for config in the working directory
	err := viper.ReadInConfig()    // Find and read the config file
	if err != nil {                // Handle errors reading the config file
		slog.Error("config file issue", "error", err)
		os.Exit(1)
	}
}

func initConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Error("Failed to get home directory", "error", err)
		return "", err
	}
	// Create the config directory if it doesn't exist
	configDir := home + "/.config/gopim"
	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		err = os.Mkdir(configDir, 0700)
		if err != nil {
			slog.Error("Failed to create config directory", "error", err)
			return "", err
		}
	}

	return configDir, nil
}

func initCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Error("Failed to get home directory", "error", err)
		return "", err
	}
	cacheDir := home + "/.cache/gopim"
	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		err = os.Mkdir(cacheDir, 0700)
		if err != nil {
			slog.Error("Failed to create cache directory", "error", err)
			return "", err
		}
	}

	return cacheDir, nil
}

func contains(slice []string, item string) bool {
	for _, a := range slice {
		if a == item {
			return true
		}
	}
	return false
}

func getRecordCache(authRecordPath string) (azidentity.AuthenticationRecord, azidentity.Cache, error) {
	record, err := retrieveRecord(authRecordPath)
	if err != nil {
		slog.Warn("Failed to retrieve authentication record", "error", err)
		return azidentity.AuthenticationRecord{}, azidentity.Cache{}, err
	} else {
		slog.Info("Using cached authentication record")
	}
	c, err := cache.New(nil)
	if err != nil {
		slog.Warn("Failed to create cache, secure caching not possible in this runtime environment", "error", err)
		slog.Info("Proceeding without persistent caching")
		return record, azidentity.Cache{}, err
	} else {
		slog.Info("Setting up persistent caching")
	}
	return record, c, err
}

func loginAzure(tenant string, cachedir string) (*azidentity.InteractiveBrowserCredential, error) {
	credentialOptions := azidentity.InteractiveBrowserCredentialOptions{
		TenantID: tenant,
	}

	var record azidentity.AuthenticationRecord
	var c azidentity.Cache
	var err error
	authRecordPath := cachedir + "/authrecord.json"
	if cachedir != "" {
		record, c, err = getRecordCache(authRecordPath)
		if err != nil {
			slog.Warn("Failed to get record cache", "error", err)
		} else {
			credentialOptions.AuthenticationRecord = record
			credentialOptions.Cache = c
		}
	}

	cred, err := azidentity.NewInteractiveBrowserCredential(&credentialOptions)
	if err != nil {
		slog.Error("Failed to interactively get credentails", "error", err)
		os.Exit(1)
	}

	if cachedir != "" {
		if record == (azidentity.AuthenticationRecord{}) {
			// No stored record; call Authenticate to acquire one.
			// This will prompt the user to authenticate interactively.
			slog.Debug("No current stored authentication record, prompting for authentication")
			newRecord, err := cred.Authenticate(context.TODO(), nil)
			if err != nil {
				slog.Error("Failed to authenticate", "error", err)
				os.Exit(1)
			} else {
				slog.Info("Successfully authenticated")
			}
			err = storeRecord(newRecord, authRecordPath)
			if err != nil {
				slog.Error("Failed to store authentication record", "error", err)
			}
		}
	}

	return cred, nil
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
	// Handle command line flags
	debug := flag.Bool("debug", false, "Debug mode")
	subs := flag.String("subs", "", "Comma separated subscription names for PIM activation (required)")
	tenant := flag.String("tenant", "", "Azure Tenant ID")
	dryrun := flag.Bool("dryrun", false, "Dry run mode, do not activate PIM")
	nocache := flag.Bool("nocache", false, "Do not use cached authentication record")
	var version bool
	flag.BoolVar(&version, "version", false, "print version information and exit")
	flag.BoolVar(&version, "v", false, "short alias for -version")

	flag.Parse()

	if version {
		fmt.Println("Version:", Version())
		os.Exit(0)
	}

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

	if *dryrun {
		slog.Info("Dry run mode enabled, not activating PIM")
	}

	if *nocache {
		slog.Info("Not using cached authentication record")
	}

	// Make sure subscriptions are provided
	if *subs == "" {
		slog.Error("No subscriptions provided")
		flag.Usage()
		os.Exit(1)
	}

	// Set up dynamic config vars
	// Initialize config directory
	configDir, err := initConfigDir()
	if err != nil {
		slog.Error("Failed to initialize config directory", "error", err)
		os.Exit(1)
	}

	// read the config file and environment variables
	readConfig(configDir)

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

	// Login to Azure
	cacheDir := ""
	if !*nocache {
		cacheDir, err = initCacheDir()
		if err != nil {
			slog.Error("Failed to initialize cache directory", "error", err)
			cacheDir = ""
		}
	}
	cred, err := loginAzure(*tenant, cacheDir)
	if err != nil {
		slog.Error("Failed to login to Azure", "error", err)
		os.Exit(1)
	}

	// Create an HTTP client with a 30s timeout
	context := context.Background()
	client := &http.Client{Timeout: 30 * time.Second}
	token, err := cred.GetToken(context, policy.TokenRequestOptions{Scopes: []string{"https://management.azure.com/.default"}})

	if err != nil {
		slog.Error("Failed to get access token", "error", err)
		os.Exit(1)
	}

	// Download the role eligibility schedule instances
	subscriptions := strings.Split(*subs, ",")
	roles, err := getRESIs(subscriptions, token.Token, *client)

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

		go func(myPrincipalID, ownerRoleID, roleEligibilityScheduleID, sub, token string, client http.Client, dryrun bool) {
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

			// Send the request if not in dry run mode
			if !dryrun {
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
			} else {
				slog.Info("Dry run mode, not activating PIM")
			}
		}(myPrincipalID, ownerRoleID, roleEligibilityScheduleID, sub, token.Token, *client, *dryrun)
	}

	wg.Wait()
}
