package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

type WatchTarget struct {
	Path          string `json:"path"`
	DriveFolderID string `json:"drive_folder_id"`
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
	config       Config
	driveService *drive.Service
	timers       = make(map[string]*time.Timer)
	timersMu     sync.Mutex
	exeDir       string
	configPath   string
	logChan      = make(chan LogMessage, 100)
	wsClients    = make(map[chan LogMessage]bool)
	wsMu         sync.RWMutex
	watcher      *fsnotify.Watcher
	httpServer   *http.Server

	// Circular buffer for recent logs
	logHistory     []LogMessage
	logHistoryMu   sync.RWMutex
	logHistorySize = 1000
)

func init() {
	exePath, _ := os.Executable()
	exeDir = filepath.Dir(exePath)
	configPath = filepath.Join(exeDir, "config.json")
	logHistory = make([]LogMessage, 0, logHistorySize)
}

func main() {
	authFlag := flag.Bool("auth", false, "Run OAuth 2.0 Web Flow")
	daemonFlag := flag.Bool("daemon", false, "Run the background watcher daemon")
	portFlag := flag.String("port", "8081", "HTTP API port")
	flag.Parse()

	// Setup logging
	logFile, _ := os.OpenFile(filepath.Join(exeDir, "cloud_daemon.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	log.SetOutput(io.MultiWriter(logFile, os.Stdout))

	loadConfig()

	// Start HTTP server for GUI communication
	go startHTTPServer(*portFlag)

	if *authFlag {
		runAuthFlow()
		return
	}

	if *daemonFlag {
		writePID()
		defer os.Remove(filepath.Join(exeDir, "cloud.pid"))
		runDaemon()
		return
	}

	fmt.Println("Use -daemon to run the watcher or -auth to authenticate.")
}

// addLog stores the message in the circular buffer and sends it to all SSE clients
func addLog(level, message string) {
	now := time.Now()
	dateStr := now.Format("2 Jan 2006") // "24 Jun 2025"
	timeStr := now.Format("15:04:05")   // "12:30:01"

	msg := LogMessage{
		Date:    dateStr,
		Time:    timeStr,
		Level:   level,
		Message: message,
	}

	// Write to log file with different format
	log.Printf("[%s] [%s] %s: %s", dateStr, timeStr, level, message)

	// Store in circular buffer
	logHistoryMu.Lock()
	if len(logHistory) >= logHistorySize {
		logHistory = logHistory[1:]
	}
	logHistory = append(logHistory, msg)
	logHistoryMu.Unlock()

	// Send to SSE clients
	wsMu.RLock()
	for client := range wsClients {
		select {
		case client <- msg:
		default:
		}
	}
	wsMu.RUnlock()
}

func startHTTPServer(port string) {
	mux := http.NewServeMux()

	// SSE streaming endpoint (keeps connection open)
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		clientChan := make(chan LogMessage, 10)
		wsMu.Lock()
		wsClients[clientChan] = true
		wsMu.Unlock()

		defer func() {
			wsMu.Lock()
			delete(wsClients, clientChan)
			wsMu.Unlock()
			close(clientChan)
		}()

		for msg := range clientChan {
			data, _ := json.Marshal(msg)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	})

	// New endpoint: get recent logs as JSON (non‑streaming)
	mux.HandleFunc("/api/logs/history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		logHistoryMu.RLock()
		defer logHistoryMu.RUnlock()
		json.NewEncoder(w).Encode(logHistory)
	})

	// Get status endpoint
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		status := map[string]interface{}{
			"running": true,
			"pid":     os.Getpid(),
			"targets": len(config.Targets),
		}
		json.NewEncoder(w).Encode(status)
	})

	// Get config endpoint
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(config)
	})

	// Update config endpoint
	mux.HandleFunc("/api/config/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		var newConfig Config
		if err := json.NewDecoder(r.Body).Decode(&newConfig); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		config = newConfig
		saveConfig()
		restartWatcher()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Add target endpoint
	mux.HandleFunc("/api/targets/add", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		var target WatchTarget
		if err := json.NewDecoder(r.Body).Decode(&target); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		config.Targets = append(config.Targets, target)
		saveConfig()
		restartWatcher()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Remove target endpoint
	mux.HandleFunc("/api/targets/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		var req struct{ Index int }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Index >= 0 && req.Index < len(config.Targets) {
			config.Targets = append(config.Targets[:req.Index], config.Targets[req.Index+1:]...)
			saveConfig()
			restartWatcher()
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Trigger backup endpoint
	mux.HandleFunc("/api/backup/trigger", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		var req struct{ TargetIndex int }
		json.NewDecoder(r.Body).Decode(&req)

		if req.TargetIndex >= 0 && req.TargetIndex < len(config.Targets) {
			go processBackup(config.Targets[req.TargetIndex])
			json.NewEncoder(w).Encode(map[string]string{"status": "backup started"})
		} else {
			json.NewEncoder(w).Encode(map[string]string{"status": "invalid target"})
		}
	})

	// Shutdown endpoint
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]string{"status": "shutting down"})
		go func() {
			time.Sleep(1 * time.Second)
			os.Exit(0)
		}()
	})

	httpServer = &http.Server{Addr: ":" + port, Handler: mux}

	addLog("INFO", fmt.Sprintf("HTTP server starting on port %s", port))
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		addLog("ERROR", fmt.Sprintf("HTTP server error: %v", err))
	}
}

func writePID() {
	pidFile := filepath.Join(exeDir, "cloud.pid")
	os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0600)
	addLog("INFO", fmt.Sprintf("PID file written: %d", os.Getpid()))
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
		addLog("INFO", "Created default configuration")
		return
	}
	defer file.Close()
	json.NewDecoder(file).Decode(&config)
	addLog("INFO", fmt.Sprintf("Loaded configuration with %d targets", len(config.Targets)))
}

func saveConfig() {
	data, _ := json.MarshalIndent(config, "", "  ")
	os.WriteFile(configPath, data, 0600)
	addLog("INFO", "Configuration saved")
}

func runDaemon() {
	addLog("INFO", "Cloud Backup Daemon starting...")

	if err := initDriveService(); err != nil {
		addLog("ERROR", fmt.Sprintf("Failed to initialize Drive service: %v", err))
		log.Fatalf("Failed to initialize Drive: %v", err)
	}
	addLog("INFO", "Google Drive service initialized")

	var err error
	watcher, err = fsnotify.NewWatcher()
	if err != nil {
		addLog("ERROR", fmt.Sprintf("Failed to create watcher: %v", err))
		log.Fatalf("Failed to create watcher: %v", err)
	}
	defer watcher.Close()

	pathMap := make(map[string]WatchTarget)

	// Perform initial backup for all targets
	addLog("INFO", "Performing initial backup for all targets...")
	for _, target := range config.Targets {
		addLog("INFO", fmt.Sprintf("Initial backup for: %s", target.Path))
		if err := processBackup(target); err != nil {
			addLog("ERROR", fmt.Sprintf("Initial backup failed for %s: %v", target.Path, err))
		} else {
			addLog("INFO", fmt.Sprintf("Initial backup completed for: %s", target.Path))
		}
	}

	// Setup file watchers
	for _, target := range config.Targets {
		absPath, err := filepath.Abs(target.Path)
		if err != nil {
			addLog("WARN", fmt.Sprintf("Invalid path %s: %v", target.Path, err))
			continue
		}

		info, err := os.Stat(absPath)
		if os.IsNotExist(err) {
			addLog("WARN", fmt.Sprintf("Path does not exist: %s", absPath))
			continue
		}

		if info.IsDir() {
			err = filepath.Walk(absPath, func(subPath string, subInfo os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if subInfo.IsDir() {
					watcher.Add(subPath)
					pathMap[subPath] = target
					addLog("INFO", fmt.Sprintf("Watching directory: %s", subPath))
				}
				return nil
			})
			if err != nil {
				addLog("ERROR", fmt.Sprintf("Error walking %s: %v", absPath, err))
			}
		} else {
			watcher.Add(absPath)
			pathMap[absPath] = target
			addLog("INFO", fmt.Sprintf("Watching file: %s", absPath))
		}
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&fsnotify.Chmod == fsnotify.Chmod {
					continue
				}

				var matchedTarget WatchTarget
				var found bool
				for watchedPath, target := range pathMap {
					if strings.HasPrefix(event.Name, watchedPath) {
						matchedTarget = target
						found = true
						break
					}
				}

				if found {
					addLog("DEBUG", fmt.Sprintf("Change detected: %s", event.Name))
					debounceUpload(matchedTarget)
				}

			case err, ok := <-watcher.Errors:
				if ok {
					addLog("ERROR", fmt.Sprintf("Watcher error: %v", err))
				}
			}
		}
	}()

	addLog("INFO", "Daemon is now running and monitoring files")
	<-done
	addLog("INFO", "Shutting down gracefully...")
}

func restartWatcher() {
	if watcher != nil {
		watcher.Close()
		addLog("INFO", "Restarting file watcher...")
		go runDaemon()
	}
}

func debounceUpload(target WatchTarget) {
	timersMu.Lock()
	defer timersMu.Unlock()

	if timer, exists := timers[target.Path]; exists {
		timer.Stop()
	}

	timers[target.Path] = time.AfterFunc(time.Duration(config.DebounceDelayMs)*time.Millisecond, func() {
		addLog("INFO", fmt.Sprintf("Change detected in: %s. Backing up...", target.Path))
		if err := processBackup(target); err != nil {
			addLog("ERROR", fmt.Sprintf("Backup failed for %s: %v", target.Path, err))
		}
	})
}

func processBackup(target WatchTarget) error {
	absPath, err := filepath.Abs(target.Path)
	if err != nil {
		return err
	}
	info, err := os.Stat(absPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("path no longer exists")
	}

	var uploadFilePath, uploadFileName string
	isZip := false

	if info.IsDir() {
		isZip = true
		uploadFileName = filepath.Base(absPath) + ".zip"
		tmpFile, err := os.CreateTemp("", "backup-*.zip")
		if err != nil {
			return err
		}
		defer os.Remove(tmpFile.Name())
		defer tmpFile.Close()

		addLog("INFO", fmt.Sprintf("Zipping directory: %s", absPath))
		if err := zipFolder(absPath, tmpFile); err != nil {
			addLog("ERROR", fmt.Sprintf("Zip failed: %v", err))
			return err
		}
		uploadFilePath = tmpFile.Name()
	} else {
		uploadFilePath = absPath
		uploadFileName = filepath.Base(absPath)
	}

	addLog("INFO", fmt.Sprintf("Uploading to Google Drive: %s", uploadFileName))
	return uploadToDrive(uploadFilePath, uploadFileName, target.DriveFolderID, isZip)
}

func uploadToDrive(localPath, filename, parentFolderID string, isZip bool) error {
	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer file.Close()

	query := fmt.Sprintf("name = '%s' and '%s' in parents and trashed = false", filename, parentFolderID)
	listCall, err := driveService.Files.List().Q(query).Spaces("drive").Do()
	if err != nil {
		return err
	}

	if len(listCall.Files) > 0 {
		existingFileID := listCall.Files[0].Id
		addLog("INFO", fmt.Sprintf("Updating existing file: %s", filename))
		_, err = driveService.Files.Update(existingFileID, &drive.File{}).Media(file).Do()
		if err == nil {
			addLog("INFO", fmt.Sprintf("Successfully updated: %s", filename))
		}
		return err
	}

	f := &drive.File{Name: filename, Parents: []string{parentFolderID}}
	_, err = driveService.Files.Create(f).Media(file).Do()
	if err == nil {
		addLog("INFO", fmt.Sprintf("Successfully uploaded: %s", filename))
	}
	return err
}

func zipFolder(source string, targetFile *os.File) error {
	archive := zip.NewWriter(targetFile)
	defer archive.Close()

	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(source, path)
		if err != nil || relPath == "." {
			return nil
		}
		header.Name = filepath.ToSlash(relPath)
		if info.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}
		writer, err := archive.CreateHeader(header)
		if err != nil || info.IsDir() {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(writer, file)
		return err
	})
}

func initDriveService() error {
	ctx := context.Background()
	b, err := os.ReadFile(config.CredentialsFile)
	if err != nil {
		return fmt.Errorf("missing credentials: %v", err)
	}

	oauthConfig, err := google.ConfigFromJSON(b, drive.DriveFileScope)
	if err != nil {
		return fmt.Errorf("invalid config: %v", err)
	}

	tokFile := config.TokenFile
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		return fmt.Errorf("no authentication token found. Please run authentication first")
	}

	client := oauthConfig.Client(ctx, tok)
	srv, err := drive.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return fmt.Errorf("drive init failed: %v", err)
	}
	driveService = srv
	return nil
}

func runAuthFlow() {
	ctx := context.Background()
	b, err := os.ReadFile(config.CredentialsFile)
	if err != nil {
		fmt.Println("Missing credentials.json")
		addLog("ERROR", "Missing credentials.json for authentication")
		return
	}

	oauthConfig, err := google.ConfigFromJSON(b, drive.DriveFileScope)
	if err != nil {
		fmt.Println("Invalid credentials.json")
		addLog("ERROR", "Invalid credentials.json")
		return
	}
	oauthConfig.RedirectURL = "http://localhost:8080"

	authURL := oauthConfig.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	exec.Command("rundll32", "url.dll,FileProtocolHandler", authURL).Start()
	fmt.Printf("Opening browser for authentication...\n")

	codeChan := make(chan string)
	server := &http.Server{Addr: ":8080"}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if code := r.URL.Query().Get("code"); code != "" {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<h2 style='font-family: sans-serif; color: green;'>Authentication Successful. You can close this window.</h2>")
			codeChan <- code
			return
		}
	})

	go func() { server.ListenAndServe() }()
	authCode := <-codeChan
	server.Shutdown(ctx)

	tok, err := oauthConfig.Exchange(ctx, authCode)
	if err != nil {
		fmt.Printf("Token exchange failed: %v\n", err)
		addLog("ERROR", fmt.Sprintf("Token exchange failed: %v", err))
		return
	}

	f, err := os.OpenFile(config.TokenFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err == nil {
		defer f.Close()
		json.NewEncoder(f).Encode(tok)
		fmt.Println("Token saved successfully!")
		addLog("INFO", "Authentication token saved successfully")
	}
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}