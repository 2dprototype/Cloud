package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gonutz/wui/v2"
)

type WatchTarget struct {
    Path          string `json:"path"`
    DriveFolderID string `json:"drive_folder_id"`
    DriveFileName string `json:"drive_file_name"`
}

type Config struct {
	CredentialsFile string        `json:"credentials_file"`
	TokenFile       string        `json:"token_file"`
	DebounceDelayMs int           `json:"debounce_delay_ms"`
	Targets         []WatchTarget `json:"targets"`
}

type LogMessage struct {
	Date    string `json:"date"`
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

var (
	config      Config
	exeDir      string
	configPath  string
	cloudExe    string
	daemonPort  = "8081"
	httpClient  = &http.Client{Timeout: 5 * time.Second}
	logBuffer   []LogMessage
	logBufferMu sync.Mutex
)

func init() {
	exePath, _ := os.Executable()
	exeDir = filepath.Dir(exePath)
	configPath = filepath.Join(exeDir, "config.json")
	cloudExe = filepath.Join(exeDir, "cloud.exe")
}

func setText(t *wui.TextEdit, text string) {
	windowsText := strings.ReplaceAll(text, "\n", "\r\n")
	t.SetText(windowsText)
}

func main() {
	loadConfig()

	w := wui.NewWindow()
	w.SetTitle("Cloud GUI")
	w.SetInnerSize(800, 460)
	w.SetPosition(120,40)
	w.SetHasMaxButton(false)
	w.SetResizable(true)
	w.SetBackground(wui.RGB(240, 240, 240))

	// Title
	titleLabel := wui.NewLabel()
	titleLabel.SetText("Cloud Backup Manager (Google Drive)")
	titleLabel.SetBounds(20, 10, 350, 24)
	titleLabel.SetFont(createBoldFont())
	w.Add(titleLabel)

	// Status Panel
	statusPanel := wui.NewPanel()
	statusPanel.SetBounds(20, 40, 760, 36)
	statusPanel.SetBorderStyle(wui.PanelBorderStyle(1))

	lblStatus := wui.NewLabel()
	lblStatus.SetBounds(10, 8, 280, 22)
	statusPanel.Add(lblStatus)

	lblPid := wui.NewLabel()
	lblPid.SetBounds(300, 8, 180, 22)
	statusPanel.Add(lblPid)

	w.Add(statusPanel)

	// Targets Table Section
	lblTargets := wui.NewLabel()
	lblTargets.SetText("Watch Targets")
	lblTargets.SetBounds(20, 86, 150, 20)
	lblTargets.SetFont(createBoldFont())
	w.Add(lblTargets)

	table := wui.NewStringTable("Local Path", "Drive Folder ID", "Drive File Name")
	table.SetBounds(20, 108, 400, 150)
	refreshTable(table)
	w.Add(table)

	// Target Management Buttons
	btnAdd := wui.NewButton()
	btnAdd.SetText("Add")
	btnAdd.SetBounds(20, 268, 80, 28)
	btnAdd.SetOnClick(func() {
		showTargetModal(w, table, -1)
	})
	w.Add(btnAdd)

	btnEdit := wui.NewButton()
	btnEdit.SetText("Edit")
	btnEdit.SetBounds(110, 268, 80, 28)
	btnEdit.SetOnClick(func() {
		idx := table.SelectedRow()
		if idx >= 0 && idx < len(config.Targets) {
			showTargetModal(w, table, idx)
		} else {
			wui.MessageBoxWarning("Notice", "Please select a target to edit.")
		}
	})
	w.Add(btnEdit)

	btnRem := wui.NewButton()
	btnRem.SetText("Remove")
	btnRem.SetBounds(200, 268, 80, 28)
	btnRem.SetOnClick(func() {
		idx := table.SelectedRow()
		if idx >= 0 && idx < len(config.Targets) {
			if wui.MessageBoxYesNo("Confirm", "Remove this target?") {
				removeTarget(idx)
				refreshTable(table)
				restartDaemonIfRunning()
			}
		}
	})
	w.Add(btnRem)

	btnTriggerBackup := wui.NewButton()
	btnTriggerBackup.SetText("Backup Now")
	btnTriggerBackup.SetBounds(290, 268, 100, 28)
	btnTriggerBackup.SetOnClick(func() {
		idx := table.SelectedRow()
		if idx >= 0 && idx < len(config.Targets) {
			triggerBackup(idx)
		} else {
			wui.MessageBoxWarning("Notice", "Please select a target to backup.")
		}
	})
	w.Add(btnTriggerBackup)

	// Log Viewer Section
	lblLogs := wui.NewLabel()
	lblLogs.SetText("Live Logs")
	lblLogs.SetBounds(430, 86, 100, 20)
	lblLogs.SetFont(createBoldFont())
	w.Add(lblLogs)

	// Smaller font for log text
	smallFont, _ := wui.NewFont(wui.FontDesc{Height: 13})
	logText := wui.NewTextEdit()
	logText.SetBounds(430, 108, 350, 150)
	logText.SetReadOnly(true)
	logText.SetWordWrap(true)
	logText.SetFont(smallFont)
	w.Add(logText)

	// Log Control Buttons
	btnClearLogs := wui.NewButton()
	btnClearLogs.SetText("Clear Logs")
	btnClearLogs.SetBounds(530, 268, 110, 28)
	btnClearLogs.SetOnClick(func() {
		logText.SetText("")
	})
	w.Add(btnClearLogs)

	btnRefreshLogs := wui.NewButton()
	btnRefreshLogs.SetText("Refresh Logs")
	btnRefreshLogs.SetBounds(650, 268, 130, 28)
	btnRefreshLogs.SetOnClick(func() {
		fetchAndDisplayLogs(logText)
	})
	w.Add(btnRefreshLogs)

	// Daemon Control Panel
	controlPanel := wui.NewPanel()
	controlPanel.SetBounds(20, 306, 760, 68)
	controlPanel.SetBorderStyle(wui.PanelBorderStyle(1))

	lblControl := wui.NewLabel()
	lblControl.SetText("Daemon Control")
	lblControl.SetBounds(10, 5, 150, 20)
	lblControl.SetFont(createBoldFont())
	controlPanel.Add(lblControl)

	btnStart := wui.NewButton()
	btnStart.SetText("Start")
	btnStart.SetBounds(10, 30, 100, 30)
	btnStart.SetOnClick(func() {
		startDaemon()
		updateStatusLabels(lblStatus, lblPid)
	})
	controlPanel.Add(btnStart)

	btnStop := wui.NewButton()
	btnStop.SetText("Stop")
	btnStop.SetBounds(120, 30, 100, 30)
	btnStop.SetOnClick(func() {
		stopDaemon()
		updateStatusLabels(lblStatus, lblPid)
	})
	controlPanel.Add(btnStop)

	btnRestart := wui.NewButton()
	btnRestart.SetText("Restart")
	btnRestart.SetBounds(230, 30, 100, 30)
	btnRestart.SetOnClick(func() {
		restartDaemon()
		updateStatusLabels(lblStatus, lblPid)
	})
	controlPanel.Add(btnRestart)

	btnKill := wui.NewButton()
	btnKill.SetText("Force Kill")
	btnKill.SetBounds(340, 30, 100, 30)
	btnKill.SetOnClick(func() {
		forceKillDaemon()
		updateStatusLabels(lblStatus, lblPid)
	})
	controlPanel.Add(btnKill)

	btnShowStatus := wui.NewButton()
	btnShowStatus.SetText("Status")
	btnShowStatus.SetBounds(450, 30, 100, 30)
	btnShowStatus.SetOnClick(func() {
		showDaemonStatus()
	})
	controlPanel.Add(btnShowStatus)

	w.Add(controlPanel)

	// Authentication Panel
	authPanel := wui.NewPanel()
	authPanel.SetBounds(20, 384, 480, 60)
	authPanel.SetBorderStyle(wui.PanelBorderStyle(1))

	lblAuth := wui.NewLabel()
	lblAuth.SetText("Google Drive Authentication")
	lblAuth.SetBounds(10, 5, 200, 20)
	lblAuth.SetFont(createBoldFont())
	authPanel.Add(lblAuth)

	btnAuth := wui.NewButton()
	btnAuth.SetText("Authenticate")
	btnAuth.SetBounds(10, 30, 140, 24)
	btnAuth.SetOnClick(func() {
		runAuthentication()
	})
	authPanel.Add(btnAuth)

	btnResetAuth := wui.NewButton()
	btnResetAuth.SetText("Reset Auth")
	btnResetAuth.SetBounds(160, 30, 120, 24)
	btnResetAuth.SetOnClick(func() {
		if wui.MessageBoxYesNo("Reset Auth", "This will delete all authentication data. Continue?") {
			resetAuthentication()
		}
	})
	authPanel.Add(btnResetAuth)

	btnLoadCreds := wui.NewButton()
	btnLoadCreds.SetText("Load Credentials")
	btnLoadCreds.SetBounds(290, 30, 150, 24)
	btnLoadCreds.SetOnClick(func() {
		loadCredentialsFile()
	})
	authPanel.Add(btnLoadCreds)

	w.Add(authPanel)

	// Autostart Panel
	autoPanel := wui.NewPanel()
	autoPanel.SetBounds(510, 384, 270, 60)
	autoPanel.SetBorderStyle(wui.PanelBorderStyle(1))

	lblAuto := wui.NewLabel()
	lblAuto.SetText("Auto Start")
	lblAuto.SetBounds(10, 5, 100, 20)
	lblAuto.SetFont(createBoldFont())
	autoPanel.Add(lblAuto)

	btnAutoStart := wui.NewButton()
	btnAutoStart.SetText("Enable Auto Start")
	btnAutoStart.SetBounds(10, 30, 120, 24)
	
	btnDisableAuto := wui.NewButton()
	btnDisableAuto.SetText("Disable")
	btnDisableAuto.SetBounds(140, 30, 120, 24)
	
	// Check current autostart status
	isAutoStartEnabled := checkAutoStart()
	if isAutoStartEnabled {
		btnAutoStart.SetText("Auto Start ON")
	} else {
		btnAutoStart.SetText("Auto Start OFF")
	}
	
	btnAutoStart.SetOnClick(func() {
		if enableAutoStart() {
			btnAutoStart.SetText("Auto Start ON")
			wui.MessageBoxInfo("Success", "Auto start enabled. Cloud backup will start with Windows.")
		} else {
			wui.MessageBoxError("Error", "Failed to enable auto start.")
		}
	})
	autoPanel.Add(btnAutoStart)
	
	btnDisableAuto.SetOnClick(func() {
		if disableAutoStart() {
			btnAutoStart.SetText("Auto Start OFF")
			wui.MessageBoxInfo("Success", "Auto start disabled.")
		} else {
			wui.MessageBoxError("Error", "Failed to disable auto start.")
		}
	})
	autoPanel.Add(btnDisableAuto)
	
	w.Add(autoPanel)

	// Start log streaming
	go streamLogs(logText)

	// Status update loop
	go func() {
		for {
			updateStatusLabels(lblStatus, lblPid)
			time.Sleep(2 * time.Second)
		}
	}()

	// Initial status update
	updateStatusLabels(lblStatus, lblPid)

	w.Show()
}

func createBoldFont() *wui.Font {
	font, _ := wui.NewFont(wui.FontDesc{Height: 12, Bold: true})
	return font
}

func updateStatusLabels(statusLabel, pidLabel *wui.Label) {
	running, pid := getDaemonStatus()
	if running {
		statusLabel.SetText("Status: RUNNING")
		pidLabel.SetText(fmt.Sprintf("PID: %d", pid))
	} else {
		statusLabel.SetText("Status: STOPPED")
		pidLabel.SetText("PID: N/A")
	}
}

func getDaemonStatus() (bool, int) {
	pidFile := filepath.Join(exeDir, "cloud.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return false, 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false, 0
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return false, 0
	}

	// Try to communicate with daemon via HTTP
	resp, err := httpClient.Get(fmt.Sprintf("http://localhost:%s/api/status", daemonPort))
	if err == nil && resp.StatusCode == 200 {
		defer resp.Body.Close()
		return true, pid
	}

	// If HTTP fails, check if process exists
	err = proc.Signal(syscall.Signal(0))
	if err != nil {
		os.Remove(pidFile)
		return false, 0
	}
	return true, pid
}

func startDaemon() {
	if running, _ := getDaemonStatus(); running {
		wui.MessageBoxInfo("Notice", "Daemon is already running.")
		return
	}

	cmd := exec.Command(cloudExe, "-daemon", "-port", daemonPort)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		wui.MessageBoxError("Error", "Failed to start daemon: "+err.Error())
		return
	}
	cmd.Process.Release()

	// Wait for daemon to start
	time.Sleep(2 * time.Second)
	wui.MessageBoxInfo("Success", "Daemon started successfully!")
}

func stopDaemon() {
	running, _ := getDaemonStatus()
	if !running {
		wui.MessageBoxInfo("Notice", "Daemon is not running.")
		return
	}

	// Try graceful shutdown via HTTP
	resp, err := httpClient.Post(fmt.Sprintf("http://localhost:%s/api/shutdown", daemonPort), "application/json", nil)
	if err == nil {
		defer resp.Body.Close()
		time.Sleep(1 * time.Second)
		wui.MessageBoxInfo("Success", "Daemon stopped gracefully!")
		return
	}

	// Fallback to kill
	forceKillDaemon()
}

func restartDaemon() {
	stopDaemon()
	time.Sleep(2 * time.Second)
	startDaemon()
}

func forceKillDaemon() {
	pidFile := filepath.Join(exeDir, "cloud.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		wui.MessageBoxWarning("Warning", "No PID file found.")
		return
	}

	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if proc, err := os.FindProcess(pid); err == nil {
		proc.Kill()
	}
	os.Remove(pidFile)
	wui.MessageBoxInfo("Success", "Daemon force killed!")
}

func showDaemonStatus() {
	running, pid := getDaemonStatus()
	if !running {
		wui.MessageBoxInfo("Status", "Daemon is not running.")
		return
	}

	// Fetch detailed status
	resp, err := httpClient.Get(fmt.Sprintf("http://localhost:%s/api/status", daemonPort))
	if err != nil {
		wui.MessageBoxError("Error", "Failed to get status: "+err.Error())
		return
	}
	defer resp.Body.Close()

	var status map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&status)

	statusText := fmt.Sprintf("PID: %d\nTargets: %v\nRunning: %v", pid, status["targets"], status["running"])
	wui.MessageBoxInfo("Daemon Status", statusText)
}

// streamLogs uses SSE to receive live logs
func streamLogs(logText *wui.TextEdit) {
	for {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://localhost:%s/api/logs", daemonPort), nil)
		req.Header.Set("Accept", "text/event-stream")
		client := &http.Client{Timeout: 0}
		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		reader := resp.Body
		buf := make([]byte, 4096)
		var lastDate string = ""

		for {
			n, err := reader.Read(buf)
			if err != nil {
				break
			}
			lines := strings.Split(string(buf[:n]), "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "data: ") {
					var msg LogMessage
					if err := json.Unmarshal([]byte(line[6:]), &msg); err == nil {
						var logEntry string
						// Print date only when it changes
						if msg.Date != lastDate {
							logEntry = fmt.Sprintf("\n[%s]\n", msg.Date)
							lastDate = msg.Date
						}
						// Add the message with time
						logEntry += fmt.Sprintf("[%s] %s: %s\n", msg.Time, msg.Level, msg.Message)

						currentText := logText.Text()
						setText(logText, currentText+logEntry)
						logText.SetCursorPosition(len(logText.Text()))
					}
				}
			}
		}
		resp.Body.Close()
		time.Sleep(2 * time.Second)
	}
}

// fetchAndDisplayLogs gets the log history (non‑streaming) and displays it
func fetchAndDisplayLogs(logText *wui.TextEdit) {
	resp, err := httpClient.Get(fmt.Sprintf("http://localhost:%s/api/logs/history", daemonPort))
	if err != nil {
		logText.SetText("Unable to fetch logs. Is daemon running?")
		return
	}
	defer resp.Body.Close()

	var logs []LogMessage
	if err := json.NewDecoder(resp.Body).Decode(&logs); err != nil {
		logText.SetText("Error parsing logs")
		return
	}

	var builder strings.Builder
	var lastDate string = ""

	for _, msg := range logs {
		// Print date only when it changes
		if msg.Date != lastDate {
			builder.WriteString(fmt.Sprintf("\n[%s]\n", msg.Date))
			lastDate = msg.Date
		}
		// Add the message with time
		builder.WriteString(fmt.Sprintf("[%s] %s: %s\n", msg.Time, msg.Level, msg.Message))
	}

	setText(logText, builder.String())
	logText.SetCursorPosition(len(logText.Text()))
}

func triggerBackup(index int) {
	if index < 0 || index >= len(config.Targets) {
		return
	}

	reqBody := bytes.NewBufferString(fmt.Sprintf(`{"target_index":%d}`, index))
	resp, err := httpClient.Post(fmt.Sprintf("http://localhost:%s/api/backup/trigger", daemonPort), "application/json", reqBody)
	if err != nil {
		wui.MessageBoxError("Error", "Failed to trigger backup: "+err.Error())
		return
	}
	defer resp.Body.Close()

	wui.MessageBoxInfo("Backup", "Backup triggered successfully!")
}

func removeTarget(index int) {
	reqBody := bytes.NewBufferString(fmt.Sprintf(`{"index":%d}`, index))
	resp, err := httpClient.Post(fmt.Sprintf("http://localhost:%s/api/targets/remove", daemonPort), "application/json", reqBody)
	if err == nil {
		defer resp.Body.Close()
	}

	// Also update local config
	if index >= 0 && index < len(config.Targets) {
		config.Targets = append(config.Targets[:index], config.Targets[index+1:]...)
		saveConfig()
	}
}

func restartDaemonIfRunning() {
	if running, _ := getDaemonStatus(); running {
		restartDaemon()
	}
}

func runAuthentication() {
	cmd := exec.Command(cloudExe, "-auth")
	cmd.Dir = exeDir
	if err := cmd.Start(); err != nil {
		wui.MessageBoxError("Error", "Failed to start authentication: "+err.Error())
		return
	}
	wui.MessageBoxInfo("Authentication", "Browser opened for authentication. Please complete the process.")
}

func resetAuthentication() {
	os.Remove(filepath.Join(exeDir, "credentials.json"))
	os.Remove(filepath.Join(exeDir, "token.json"))
	wui.MessageBoxInfo("Reset", "Authentication data cleared. Please reload credentials and authenticate again.")
}

func loadCredentialsFile() {
	fd := wui.NewFileOpenDialog()
	fd.SetTitle("Select credentials.json")
	fd.AddFilter("JSON Files", ".json")
	if ok, path := fd.ExecuteSingleSelection(nil); ok {
		destPath := filepath.Join(exeDir, "credentials.json")
		if err := copyFile(path, destPath); err != nil {
			wui.MessageBoxError("Error", "Failed to copy credentials: "+err.Error())
		} else {
			wui.MessageBoxInfo("Success", "Credentials loaded successfully!")
		}
	}
}

func refreshTable(table *wui.StringTable) {
    table.Clear()
    for row, t := range config.Targets {
        table.SetCell(0, row, t.Path)
        table.SetCell(1, row, t.DriveFolderID)
        displayName := t.DriveFileName
        if displayName == "" {
            displayName = "(use original name)"
        }
        table.SetCell(2, row, displayName)
    }
}

func loadConfig() {
	file, err := os.Open(configPath)
	if err != nil {
		config = Config{
			CredentialsFile: filepath.Join(exeDir, "credentials.json"),
			TokenFile:       filepath.Join(exeDir, "token.json"),
			DebounceDelayMs: 3000,
			Targets:         []WatchTarget{},
		}
		saveConfig()
		return
	}
	defer file.Close()
	json.NewDecoder(file).Decode(&config)
}

func saveConfig() {
	data, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(configPath, data, 0600)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// showTargetModal creates a modal dialog to add or edit a watch target
func showTargetModal(parent *wui.Window, table *wui.StringTable, editIndex int) {
    dlg := wui.NewWindow()
    title := "Add Target"
    if editIndex >= 0 {
        title = "Edit Target"
    }
    dlg.SetTitle(title)
    dlg.SetInnerSize(450, 300)
	dlg.SetPosition(270, 120)
    dlg.SetResizable(false)

    // Path selection
    lblPath := wui.NewLabel()
    lblPath.SetText("Local Folder/File Path:")
    lblPath.SetBounds(20, 20, 200, 20)
    dlg.Add(lblPath)

    pathEdit := wui.NewEditLine()
    pathEdit.SetBounds(20, 40, 320, 25)
    dlg.Add(pathEdit)

    // Browse Folder button
    btnBrowseFolder := wui.NewButton()
    btnBrowseFolder.SetText("Folder")
    btnBrowseFolder.SetBounds(350, 40, 80, 25)
    btnBrowseFolder.SetOnClick(func() {
        fd := wui.NewFolderSelectDialog()
        fd.SetTitle("Select Folder to Watch")
        if ok, path := fd.Execute(dlg); ok {
            pathEdit.SetText(path)
        }
    })
    dlg.Add(btnBrowseFolder)

    // Browse File button (below the folder browse button)
    btnBrowseFile := wui.NewButton()
    btnBrowseFile.SetText("File")
    btnBrowseFile.SetBounds(350, 70, 80, 25)
    btnBrowseFile.SetOnClick(func() {
        fd := wui.NewFileOpenDialog()
        fd.SetTitle("Select File to Watch")
        fd.AddFilter("All Files", "*.*")
        if ok, path := fd.ExecuteSingleSelection(dlg); ok {
            pathEdit.SetText(path)
        }
    })
    dlg.Add(btnBrowseFile)

    // Drive Folder ID
    lblDriveID := wui.NewLabel()
    lblDriveID.SetText("Google Drive Folder ID:")
    lblDriveID.SetBounds(20, 100, 200, 20)
    dlg.Add(lblDriveID)

    idEdit := wui.NewEditLine()
    idEdit.SetBounds(20, 120, 410, 25)
    dlg.Add(idEdit)

    // Custom Drive File Name
    lblFileName := wui.NewLabel()
    lblFileName.SetText("Drive File Name (optional):")
    lblFileName.SetBounds(20, 160, 200, 20)
    dlg.Add(lblFileName)

    fileNameEdit := wui.NewEditLine()
    fileNameEdit.SetBounds(20, 180, 410, 25)
    fileNameEdit.SetText("") // Empty means use default name
    dlg.Add(fileNameEdit)

    lblHint := wui.NewLabel()
    lblHint.SetText("Leave empty to use original name")
    lblHint.SetBounds(20, 210, 300, 15)
    smallFont, _ := wui.NewFont(wui.FontDesc{Height: 13})
    lblHint.SetFont(smallFont)
    dlg.Add(lblHint)

    if editIndex >= 0 {
        pathEdit.SetText(config.Targets[editIndex].Path)
        idEdit.SetText(config.Targets[editIndex].DriveFolderID)
        fileNameEdit.SetText(config.Targets[editIndex].DriveFileName)
    }

    // Buttons
    btnSave := wui.NewButton()
    btnSave.SetText("Save")
    btnSave.SetBounds(120, 240, 100, 30)
    btnSave.SetOnClick(func() {
        if pathEdit.Text() == "" || idEdit.Text() == "" {
            wui.MessageBoxError("Error", "Local Path and Drive Folder ID are required.")
            return
        }
        newTarget := WatchTarget{
            Path:          pathEdit.Text(),
            DriveFolderID: idEdit.Text(),
            DriveFileName: fileNameEdit.Text(),
        }
        if editIndex >= 0 {
            config.Targets[editIndex] = newTarget
        } else {
            config.Targets = append(config.Targets, newTarget)
        }
        saveConfig()
        refreshTable(table)
        restartDaemonIfRunning()
        dlg.Close()
    })
    dlg.Add(btnSave)

    btnCancel := wui.NewButton()
    btnCancel.SetText("Cancel")
    btnCancel.SetBounds(230, 240, 100, 30)
    btnCancel.SetOnClick(func() {
        dlg.Close()
    })
    dlg.Add(btnCancel)

    dlg.ShowModal()
}


// Autostart functions for Windows
func getStartupFolderPath() string {
	startupFolder := os.Getenv("APPDATA") + "\\Microsoft\\Windows\\Start Menu\\Programs\\Startup"
	return startupFolder
}

func getShortcutPath() string {
	return filepath.Join(getStartupFolderPath(), "CloudBackup.lnk")
}

func enableAutoStart() bool {
	// Create VBS script to create shortcut
	vbsContent := `Set WshShell = CreateObject("WScript.Shell")
strDesktop = WshShell.SpecialFolders("Startup")
Set oShellLink = WshShell.CreateShortcut(strDesktop & "\CloudBackup.lnk")
oShellLink.TargetPath = "` + cloudExe + `"
oShellLink.Arguments = "-daemon -port 8081"
oShellLink.WorkingDirectory = "` + exeDir + `"
oShellLink.Description = "Cloud Backup Daemon"
oShellLink.WindowStyle = 7
oShellLink.Save`

	vbsPath := filepath.Join(os.TempDir(), "create_shortcut.vbs")
	err := os.WriteFile(vbsPath, []byte(vbsContent), 0644)
	if err != nil {
		return false
	}
	defer os.Remove(vbsPath)

	cmd := exec.Command("cscript", "//Nologo", vbsPath)
	err = cmd.Run()
	if err != nil {
		return false
	}

	return true
}

func disableAutoStart() bool {
	shortcutPath := getShortcutPath()
	err := os.Remove(shortcutPath)
	return err == nil
}

func checkAutoStart() bool {
	shortcutPath := getShortcutPath()
	_, err := os.Stat(shortcutPath)
	return err == nil
}