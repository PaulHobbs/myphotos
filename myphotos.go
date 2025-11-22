package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	"github.com/pelletier/go-toml/v2"
)

// --- Configuration Structures ---

type Config struct {
	Extensions []string `toml:"extensions"`
}

func getDefaultConfig() Config {
	return Config{
		Extensions: []string{".jpg", ".JPG", ".ARW", ".mp4", ".MP4"},
	}
}

func getDefaultDBPath() string {
	usr, err := user.Current()
	if err != nil {
		return "db.sqlite"
	}
	return filepath.Join(usr.HomeDir, ".config", "myphotos", "db.sqlite")
}

// --- Database Logic ---

func initDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	// Create table if it doesn't exist
	// We use filename + size as the unique identifier
	query := `
	CREATE TABLE IF NOT EXISTS photos (
		filename TEXT,
		size INTEGER,
		local_path TEXT,
		remote_path TEXT,
		PRIMARY KEY (filename, size)
	);`

	_, err = db.Exec(query)
	if err != nil {
		return nil, err
	}

	return db, nil
}

func upsertLocal(db *sql.DB, filename string, size int64, path string) error {
	query := `
	INSERT INTO photos (filename, size, local_path) VALUES (?, ?, ?)
	ON CONFLICT(filename, size) DO UPDATE SET local_path=excluded.local_path;
	`
	_, err := db.Exec(query, filename, size, path)
	return err
}

func upsertRemote(db *sql.DB, filename string, size int64, path string) error {
	query := `
	INSERT INTO photos (filename, size, remote_path) VALUES (?, ?, ?)
	ON CONFLICT(filename, size) DO UPDATE SET remote_path=excluded.remote_path;
	`
	_, err := db.Exec(query, filename, size, path)
	return err
}

// --- Configuration Management ---

func loadOrCreateConfig() (*Config, error) {
	usr, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("could not get user home: %v", err)
	}

	configDir := filepath.Join(usr.HomeDir, ".config", "myphotos")
	configFile := filepath.Join(configDir, "extensions.toml")

	// Check if file exists
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		// Create default
		if err := os.MkdirAll(configDir, 0755); err != nil {
			return nil, err
		}

		cfg := getDefaultConfig()
		data, err := toml.Marshal(cfg)
		if err != nil {
			return nil, err
		}

		if err := os.WriteFile(configFile, data, 0644); err != nil {
			return nil, err
		}
		fmt.Printf("Created default config at %s\n", configFile)
		return &cfg, nil
	}

	// Read existing
	data, err := os.ReadFile(configFile)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// --- Main Logic ---

func isExtensionAllowed(path string, extensions []string) bool {
	for _, ext := range extensions {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

func runAdd(args []string, cfg *Config) {
	defaultDB := getDefaultDBPath()
	addCmd := flag.NewFlagSet("add", flag.ExitOnError)
	remotePtr := addCmd.String("remote", "", "The remote server address (e.g. user@192.168.1.100). If empty, scans local.")
	pathPtr := addCmd.String("path", "", "The directory path to scan")
	dbPtr := addCmd.String("db", defaultDB, "Path to the sqlite database file")

	addCmd.Parse(args)

	if *pathPtr == "" {
		fmt.Println("Error: -path is required")
		addCmd.PrintDefaults()
		os.Exit(1)
	}

	db, err := initDB(*dbPtr)
	if err != nil {
		log.Fatalf("Failed to initialize DB: %v", err)
	}
	defer db.Close()

	// --- Remote Scan (via SSH) ---
	if *remotePtr != "" {
		fmt.Printf("Scanning REMOTE [%s] at path [%s]...\n", *remotePtr, *pathPtr)
		
		// We construct a find command to run over SSH.
		// We use -printf to get filename, size, and full path separated by tabs.
		sshCmd := exec.Command("ssh", *remotePtr, "find", *pathPtr, "-type", "f", "-printf", "'%f\t%s\t%p\n'")
		
		// Capture output
		var out bytes.Buffer
		var stderr bytes.Buffer
		sshCmd.Stdout = &out
		sshCmd.Stderr = &stderr

		err := sshCmd.Run()
		if err != nil {
			log.Fatalf("SSH command failed: %v\nStderr: %s", err, stderr.String())
		}

		scanner := bufio.NewScanner(&out)
		count := 0
		tx, _ := db.Begin() // Use transaction for speed

		for scanner.Scan() {
			line := scanner.Text()
			parts := strings.Split(line, "\t")
			if len(parts) != 3 {
				continue
			}
			name := parts[0]
			size, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				continue
			}
			fullPath := parts[2]

			if !isExtensionAllowed(name, cfg.Extensions) {
				continue
			}

			if err := upsertRemote(db, name, size, fullPath); err != nil {
				log.Printf("Error inserting remote file %s: %v", name, err)
			}
			count++
			if count%100 == 0 {
				fmt.Printf("\rProcessed %d remote files...", count)
			}
		}
		tx.Commit()
		fmt.Printf("\nComplete. Processed %d matching remote files.\n", count)

	} else {
		// --- Local Scan ---
		fmt.Printf("Scanning LOCAL path [%s]...\n", *pathPtr)

		count := 0
		err := filepath.WalkDir(*pathPtr, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}

			if !isExtensionAllowed(path, cfg.Extensions) {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return err
			}
			absPath, _ := filepath.Abs(path)

			if err := upsertLocal(db, d.Name(), info.Size(), absPath); err != nil {
				return err
			}
			
			count++
			if count%100 == 0 {
				fmt.Printf("\rProcessed %d local files...", count)
			}
			return nil
		})

		if err != nil {
			log.Fatalf("Error walking local path: %v", err)
		}
		fmt.Printf("\nComplete. Processed %d matching local files.\n", count)
	}
}

func runReport(args []string) {
	defaultDB := getDefaultDBPath()
	reportCmd := flag.NewFlagSet("report", flag.ExitOnError)
	dbPtr := reportCmd.String("db", defaultDB, "Path to the sqlite database file")
	verbosePtr := reportCmd.Bool("verbose", false, "Show full list of files")
	vPtr := reportCmd.Bool("v", false, "Show full list of files (shorthand)")
	wrongSizePtr := reportCmd.Bool("wrong_size", false, "Report files present on remote but with different size")
	reportCmd.Parse(args)

	isVerbose := *verbosePtr || *vPtr

	db, err := initDB(*dbPtr)
	if err != nil {
		log.Fatalf("Failed to open DB: %v", err)
	}
	defer db.Close()

	if *wrongSizePtr {
		query := `
			SELECT p1.local_path, p1.size, p2.remote_path, p2.size
			FROM photos p1
			JOIN photos p2 ON p1.filename = p2.filename
			WHERE p1.local_path IS NOT NULL AND p1.local_path != ''
			  AND (p1.remote_path IS NULL OR p1.remote_path = '')
			  AND p2.remote_path IS NOT NULL AND p2.remote_path != ''
		`
		rows, err := db.Query(query)
		if err != nil {
			log.Fatal(err)
		}
		defer rows.Close()

		fmt.Println("--- Files with Name Match but Size Mismatch ---")
		count := 0
		for rows.Next() {
			var lPath, rPath string
			var lSize, rSize int64
			if err := rows.Scan(&lPath, &lSize, &rPath, &rSize); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("Local:  %s (%d bytes)\nRemote: %s (%d bytes)\n\n", lPath, lSize, rPath, rSize)
			count++
		}
		fmt.Printf("-----------------------------------------------\n")
		fmt.Printf("Total Mismatches: %d\n", count)
		return
	}

	// Query for files that are local (backed up?) but NOT on remote.
	// We check if local_path is set and remote_path is NULL or empty.
	
	rows, err := db.Query("SELECT local_path FROM photos WHERE local_path IS NOT NULL AND local_path != '' AND (remote_path IS NULL OR remote_path = '')")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	if isVerbose {
		fmt.Println("--- Files on Local Machine but MISSING from Remote (Backup needed) ---")
		count := 0
		for rows.Next() {
			var p string
			rows.Scan(&p)
			fmt.Println(p)
			count++
		}
		fmt.Printf("---------------------------------------------------------------------\n")
		fmt.Printf("Total Missing from Remote: %d\n", count)
	} else {
		summary := make(map[string]int)
		count := 0
		for rows.Next() {
			var p string
			rows.Scan(&p)
			// Group by directory
			dir := filepath.Dir(p)
			summary[dir]++
			count++
		}

		fmt.Println("--- Summary of Missing Files (by Directory) ---")
		var keys []string
		for k := range summary {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("%s: %d\n", k, summary[k])
		}
		fmt.Printf("---------------------------------------------------------\n")
		fmt.Printf("Total Missing from Remote: %d\n", count)
		fmt.Println("(Use -v or --verbose to see full file list)")
	}
}

func printHelp() {
	fmt.Println("Usage: go run myphotos.go <command> [flags]")
	fmt.Println("\nCommands:")
	fmt.Println("  add     Scan and add files to the database")
	fmt.Println("  report  Generate a report of missing files")
	fmt.Println("  help    Show this help message")
}

func main() {
	cfg, err := loadOrCreateConfig()
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}

	if len(os.Args) < 2 {
		printHelp()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "add":
		runAdd(os.Args[2:], cfg)
	case "report":
		runReport(os.Args[2:])
	case "help", "--help", "-h":
		printHelp()
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		printHelp()
		os.Exit(1)
	}
}
