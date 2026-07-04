package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// defaultPlistLabel is the launchd label used when the user does not supply one.
// Override it at install time when prompted (e.g. with your own reverse-DNS namespace).
const defaultPlistLabel = "com.protomolecule.daemon"

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{LABEL}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{BINARY_PATH}}</string>
        <string>server</string>
        <string>--config</string>
        <string>{{CONFIG_PATH}}</string>
    </array>
    <key>WorkingDirectory</key>
    <string>{{PROJECT_ROOT}}</string>
    <key>EnvironmentVariables</key>
    <dict>
{{ENV_VARS}}    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>{{RUNTIME_DIR}}/logs/protomolecule.stderr.log</string>
    <key>StandardErrorPath</key>
    <string>{{RUNTIME_DIR}}/logs/protomolecule.stderr.log</string>
</dict>
</plist>
`

func installPlist() error {
	scanner := bufio.NewScanner(os.Stdin)

	// Prompt for project root
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	absCwd, _ := filepath.Abs(cwd)

	fmt.Printf("Project root path [%s]: ", absCwd)
	scanner.Scan()
	projectRoot := strings.TrimSpace(scanner.Text())
	if projectRoot == "" {
		projectRoot = absCwd
	}
	projectRoot, err = filepath.Abs(projectRoot)
	if err != nil {
		return fmt.Errorf("resolving project root: %w", err)
	}

	if _, err := os.Stat(projectRoot); os.IsNotExist(err) {
		return fmt.Errorf("project root does not exist: %s", projectRoot)
	}

	// Prompt for binary path
	defaultBinaryPath := filepath.Join(projectRoot, "bin", "protomolecule")
	fmt.Printf("Binary path [%s]: ", defaultBinaryPath)
	scanner.Scan()
	binaryPath := strings.TrimSpace(scanner.Text())
	if binaryPath == "" {
		binaryPath = defaultBinaryPath
	}
	binaryPath, err = filepath.Abs(binaryPath)
	if err != nil {
		return fmt.Errorf("resolving binary path: %w", err)
	}

	// Prompt for config path (defaults to the same location the daemon uses)
	cfgDefault := defaultConfigPath()
	fmt.Printf("Config file path [%s]: ", cfgDefault)
	scanner.Scan()
	cfgPath := strings.TrimSpace(scanner.Text())
	if cfgPath == "" {
		cfgPath = cfgDefault
	}
	cfgPath, err = filepath.Abs(cfgPath)
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		return fmt.Errorf("config file does not exist: %s", cfgPath)
	}

	// Prompt for launchd label (reverse-DNS convention, e.g. com.example.protomolecule)
	fmt.Printf("Launchd label [%s]: ", defaultPlistLabel)
	scanner.Scan()
	plistLabel := strings.TrimSpace(scanner.Text())
	if plistLabel == "" {
		plistLabel = defaultPlistLabel
	}
	if strings.ContainsAny(plistLabel, " \t/") {
		return fmt.Errorf("invalid launchd label %q: must not contain spaces or slashes", plistLabel)
	}

	// Build sorted list of env vars, excluding PATH (always included separately)
	envMap := make(map[string]string)
	for _, entry := range os.Environ() {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 || parts[0] == "PATH" {
			continue
		}
		envMap[parts[0]] = parts[1]
	}
	var envKeys []string
	for k := range envMap {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)

	// Walk through each var one at a time: y to include, n/Enter to skip, q to stop
	fmt.Println("\nSelect environment variables to include in the plist.")
	fmt.Println("PATH is always included automatically.")
	fmt.Println("Press y to include, n or Enter to skip, q to stop.")

	selectedEnv := make(map[string]string)
	for _, k := range envKeys {
		fmt.Printf("  Include %s? [y/N/q]: ", k)
		scanner.Scan()
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		if answer == "q" {
			break
		}
		if answer == "y" || answer == "yes" {
			selectedEnv[k] = envMap[k]
		}
	}

	// Always include PATH
	currentPath := os.Getenv("PATH")
	if currentPath == "" {
		currentPath = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}

	// Build the EnvironmentVariables XML block
	var envXML strings.Builder
	envXML.WriteString(fmt.Sprintf("        <key>PATH</key>\n        <string>%s</string>\n", currentPath))
	var selectedKeys []string
	for k := range selectedEnv {
		selectedKeys = append(selectedKeys, k)
	}
	sort.Strings(selectedKeys)
	for _, k := range selectedKeys {
		envXML.WriteString(fmt.Sprintf("        <key>%s</key>\n        <string>%s</string>\n", k, selectedEnv[k]))
	}

	// Summary
	fmt.Printf("\nEnvironment variables to be included:\n")
	fmt.Printf("  PATH\n")
	for _, k := range selectedKeys {
		fmt.Printf("  %s\n", k)
	}

	// Generate plist content
	plistContent := plistTemplate
	plistContent = strings.ReplaceAll(plistContent, "{{LABEL}}", plistLabel)
	plistContent = strings.ReplaceAll(plistContent, "{{BINARY_PATH}}", binaryPath)
	plistContent = strings.ReplaceAll(plistContent, "{{CONFIG_PATH}}", cfgPath)
	plistContent = strings.ReplaceAll(plistContent, "{{PROJECT_ROOT}}", projectRoot)
	plistContent = strings.ReplaceAll(plistContent, "{{RUNTIME_DIR}}", runtimeDir())
	plistContent = strings.ReplaceAll(plistContent, "{{ENV_VARS}}", envXML.String())

	// Create the runtime logs directory — this is where the daemon writes
	// protomolecule.log (and where launchd sends its stderr sink), matching the
	// fixed runtime dir the daemon and `logs`/`watch`/`status` all use.
	logsDir := filepath.Join(runtimeDir(), "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return fmt.Errorf("creating logs directory: %w", err)
	}

	// Write plist to ~/Library/LaunchAgents/
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("getting home directory: %w", err)
	}
	launchAgentsDir := filepath.Join(homeDir, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgentsDir, 0755); err != nil {
		return fmt.Errorf("creating LaunchAgents directory: %w", err)
	}

	installedPlistPath := filepath.Join(launchAgentsDir, plistLabel+".plist")

	// If plist already exists, confirm before overwriting
	if _, err := os.Stat(installedPlistPath); err == nil {
		fmt.Printf("\nPlist already exists at %s. Overwrite? [y/N]: ", installedPlistPath)
		scanner.Scan()
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		if answer != "y" && answer != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
		fmt.Println("Unloading existing plist...")
		unloadCmd := exec.Command("launchctl", "unload", installedPlistPath)
		if out, err := unloadCmd.CombinedOutput(); err != nil {
			fmt.Printf("Warning: unload returned error (may not have been loaded): %v\n%s\n", err, string(out))
		}
	}

	if err := os.WriteFile(installedPlistPath, []byte(plistContent), 0644); err != nil {
		return fmt.Errorf("writing plist: %w", err)
	}
	fmt.Printf("Plist written to %s\n", installedPlistPath)

	// Offer to open in editor before loading
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	fmt.Printf("Open in editor (%s) before loading? [y/N]: ", editor)
	scanner.Scan()
	if answer := strings.TrimSpace(strings.ToLower(scanner.Text())); answer == "y" || answer == "yes" {
		editCmd := exec.Command(editor, installedPlistPath)
		editCmd.Stdin = os.Stdin
		editCmd.Stdout = os.Stdout
		editCmd.Stderr = os.Stderr
		if err := editCmd.Run(); err != nil {
			fmt.Printf("Warning: editor exited with error: %v\n", err)
		}
	}

	fmt.Println("Loading plist...")
	loadCmd := exec.Command("launchctl", "load", installedPlistPath)
	if out, err := loadCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("loading plist: %v\n%s", err, string(out))
	}

	fmt.Printf("Protomolecule installed and loaded as %s\n", plistLabel)
	return nil
}
