package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Pallinder/go-randomdata"
	"github.com/cretz/bine/tor"
	"github.com/ipsn/go-libtor"
	"github.com/natefinch/lumberjack"
	"onionbox/onion_buffer"
	"onionbox/templates"
)

type onionbox struct {
	version       string
	port          int
	debug         bool
	logger        *log.Logger
	store         *onion_buffer.OnionStore
	maxFormMemory int64
	torVersion3   bool
	onionURL      string
	chunkSize     int64
}

var downloadURLreg = regexp.MustCompile(`((?:[a-z][a-z]+))`)

func main() {
	// Create onionbox instance that stores config
	ob := onionbox{
		version: "v0.1.0",
		logger:  log.New(os.Stdout, "[onionbox] ", log.LstdFlags),
		store:   onion_buffer.NewStore(),
	}
	// Init flags
	flag.BoolVar(&ob.debug, "debug", false, "run in debug mode")
	flag.BoolVar(&ob.torVersion3, "torv3", true, "use version 3 of the Tor circuit")
	flag.Int64Var(&ob.maxFormMemory, "mem", 512, "max memory allotted for handling form file buffers")
	flag.Int64Var(&ob.chunkSize, "chunks", 1024, "size of chunks for buffer I/O")
	flag.IntVar(&ob.port, "port", 80, "port to expose the onion service on")
	// Parse flags
	flag.Parse()

	// If debug is NOT enabled, write all logs to disk (instead of stdout)
	// and rotate them when necessary.
	if !ob.debug {
		ob.logger.SetOutput(&lumberjack.Logger{
			Filename:   "/var/log/onionbox/onionbox.log",
			MaxSize:    100, // megabytes
			MaxBackups: 3,
			MaxAge:     28, // days
			Compress:   true,
		})
	}

	// Create a separate go routine which infinitely loops through the store to check for
	// expired buffer entries, and delete them.
	go func() {
		if err := ob.store.DestroyExpiredBuffers(); err != nil {
			ob.logf("Error destroying expired buffers: %v", err)
		}
	}()

	// Get running OS
	var useEmbeddedCon bool
	if runtime.GOOS == "windows" {
		useEmbeddedCon = false
	} else {
		useEmbeddedCon = true
	}

	// Start tor
	ob.logf("Starting and registering onion service, please wait...")
	t, err := tor.Start(nil, &tor.StartConf{
		ProcessCreator: libtor.Creator,
		DebugWriter:    os.Stderr,
		// This option is not supported on Windows
		UseEmbeddedControlConn: useEmbeddedCon,
	})
	if err != nil {
		ob.logf("Failed to start Tor: %v", err)
		ob.quit()
	}
	defer func() {
		if err := t.Close(); err != nil {
			ob.logf("Error closing connection to Tor: %v", err)
			ob.quit()
		}
	}()

	// Wait at most 3 minutes to publish the service
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Create an onion service to listen on any port but show as 80
	onionSvc, err := t.Listen(ctx, &tor.ListenConf{
		RemotePorts: []int{ob.port},
		Version3:    ob.torVersion3,
	})
	if err != nil {
		ob.logf("Error creating the onion service: %v", err)
		ob.quit()
	}
	defer func() {
		if err := onionSvc.Close(); err != nil {
			ob.logf("Error closing connection to onion service: %v", err)
			ob.quit()
		}
	}()

	// Display the onion service URL
	ob.onionURL = onionSvc.ID
	ob.logf("Please open a Tor capable browser and navigate to http://%v.onion\n", ob.onionURL)

	// Init serving
	http.HandleFunc("/", ob.router)
	srv := &http.Server{
		// TODO: comeback. Tor is quite slow and depending on the size of the files being
		//  transferred, the server could timeout. I would like to keep set timeouts, but
		//  will need to find a sweet spot or enable an option for large transfers.
		IdleTimeout:  time.Second * 60,
		ReadTimeout:  time.Second * 60,
		WriteTimeout: time.Minute * 10,
		Handler:      nil,
	}
	// Begin serving
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(onionSvc) }()
	if err = <-errCh; err != nil {
		ob.logf("Error serving on onion service: %v", err)
		ob.quit()
	}
	// Proper server shutdown when program ends
	defer func() {
		if err := srv.Shutdown(context.Background()); err != nil {
			ob.logf("Error shutting down onionbox: %v", err)
			ob.quit()
		}
	}()
}

func (ob *onionbox) router(w http.ResponseWriter, r *http.Request) {
	// If base URL, send to URL handler
	if r.URL.Path == "/" {
		ob.upload(w, r)
	} else if matches := downloadURLreg.FindStringSubmatch(r.URL.Path); matches != nil {
		if ob.store != nil {
			if ob.store.Exists(r.URL.Path[1:]) {
				r.Header.Set("filename", r.URL.Path[1:])
				ob.download(w, r)
			}
		} else {
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
	} else {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}
}

func (ob *onionbox) upload(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		csrf, err := createCSRF()
		if err != nil {
			ob.logf("Error creating CSRF token: %v", err)
			http.Error(w, "Error displaying web page, please try refreshing.", http.StatusInternalServerError)
			return
		}
		// Parse template
		t, err := template.New("upload").Parse(templates.UploadHTML)
		if err != nil {
			ob.logf("Error loading template: %v", err)
			http.Error(w, "Error displaying web page, please try refreshing.", http.StatusInternalServerError)
			return
		}
		// Execute template
		if err := t.Execute(w, csrf); err != nil {
			ob.logf("Error executing template: %v", err)
			http.Error(w, "Error displaying web page, please try refreshing.", http.StatusInternalServerError)
			return
		}
	case http.MethodPost:
		// Parse file(s) from form
		if err := r.ParseMultipartForm(ob.maxFormMemory << 20); err != nil {
			ob.logf("Error parsing files from form: %v", err)
			http.Error(w, "Error parsing files.", http.StatusInternalServerError)
			return
		}
		files := r.MultipartForm.File["files"]
		// A buffered channel that we can send work requests on.
		// TODO: is 100 the correct value to have here?
		uploadQueue := make(chan *multipart.FileHeader, 100)
		// Loop through files attached in form and offload to uploadQueue channel
		for _, fileHeader := range files {
			uploadQueue <- fileHeader
		}
		// Create buffer for session in-memory zip file
		zipBuffer := new(bytes.Buffer)
		// Lock memory allotted to zipBuffer from being used in SWAP
		if err := syscall.Mlock(zipBuffer.Bytes()); err != nil {
			ob.logf("Error mlocking allotted memory for zipBuffer: %v", err)
		}
		// Create new zip writer
		zWriter := zip.NewWriter(zipBuffer)
		// Wait group for sync
		var wg sync.WaitGroup
		wg.Add(1)
		// Write all files in queue to memory
		go func() {
			if err := ob.writeFilesToBuffers(zWriter, uploadQueue, &wg); err != nil {
				ob.logf("Error writing files in queue to memory: %v", err)
				http.Error(w, "Error writing your files to memory.", http.StatusInternalServerError)
			}
		}()
		// Wait for zip to be finished
		wg.Wait()
		// Close uploadQueue channel after upload done
		close(uploadQueue)
		// Close zipwriter
		if err := zWriter.Close(); err != nil {
			ob.logf("Error closing zip writer: %v", err)
		}
		// Create OnionBuffer object
		oBuffer := &onion_buffer.OnionBuffer{Name: strings.ToLower(randomdata.SillyName()), ChunkSize: ob.chunkSize}
		// If password option was enabled
		if r.FormValue("password_enabled") == "on" {
			var err error
			pass := r.FormValue("password")
			oBuffer.Bytes, err = onion_buffer.Encrypt(zipBuffer.Bytes(), pass)
			if err != nil {
				ob.logf("Error encrypting buffer: %v", err)
				http.Error(w, "Error encrypting buffer.", http.StatusInternalServerError)
				return
			}
			// Lock memory allotted to oBuffer from being used in SWAP
			if err := syscall.Mlock(oBuffer.Bytes); err != nil {
				ob.logf("Error mlocking allotted memory for oBuffer: %v", err)
			}
			oBuffer.Encrypted = true
			chksm, err := oBuffer.GetChecksum()
			if err != nil {
				ob.logf("Error getting checksum: %v", err)
				http.Error(w, "Error getting checksum.", http.StatusInternalServerError)
				return
			}
			oBuffer.Checksum = chksm
		} else {
			oBuffer.Bytes = zipBuffer.Bytes()
			// Lock memory allotted to oBuffer from being used in SWAP
			if err := syscall.Mlock(oBuffer.Bytes); err != nil {
				ob.logf("Error mlocking allotted memory for oBuffer: %v", err)
			}
			// Get checksum
			chksm, err := oBuffer.GetChecksum()
			if err != nil {
				ob.logf("Error getting checksum: %v", err)
				http.Error(w, "Error getting checksum.", http.StatusInternalServerError)
				return
			}
			oBuffer.Checksum = chksm
		}
		// If limit downloads was enabled
		if r.FormValue("limit_downloads") == "on" {
			form := r.FormValue("download_limit")
			limit, err := strconv.Atoi(form)
			if err != nil {
				ob.logf("Error converting duration string into time.Duration: %v", err)
				http.Error(w, "Error getting expiration time.", http.StatusInternalServerError)
				return
			}
			oBuffer.DownloadLimit = int64(limit)
		}
		// if expiration was enabled
		if r.FormValue("expire") == "on" {
			expiration := fmt.Sprintf("%sm", r.FormValue("expiration_time"))
			if err := oBuffer.SetExpiration(expiration); err != nil {
				ob.logf("Error parsing expiration time: %v", err)
				http.Error(w, "Error parsing expiration time.", http.StatusInternalServerError)
				return
			}
		}
		// Add OnionBuffer to store
		if err := ob.store.Add(oBuffer); err != nil {
			ob.logf("Error adding file to store: %v", err)
			http.Error(w, "Error adding file to store.", http.StatusInternalServerError)
			return
		}
		// Destroy temp OnionBuffer
		if err := oBuffer.Destroy(); err != nil {
			ob.logf("Error destroying temporary var for %s", oBuffer.Name)
		}
		// Write the zip's URL to client for sharing
		_, err := w.Write([]byte(fmt.Sprintf("Files uploaded. Please share this link with your recipients: http://%s.onion/%s",
			ob.onionURL, oBuffer.Name)))
		if err != nil {
			ob.logf("Error writing to client: %v", err)
			http.Error(w, "Error writing to client.", http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "Invalid HTTP Method.", http.StatusMethodNotAllowed)
		return
	}
}

func (ob *onionbox) download(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		oBuffer := ob.store.Get(r.Header.Get("filename"))
		if oBuffer.Encrypted {
			csrf, err := createCSRF()
			if err != nil {
				ob.logf("Error creating CSRF token: %v", err)
				http.Error(w, "Error displaying web page, please try refreshing.", http.StatusInternalServerError)
				return
			}
			// Parse template
			t, err := template.New("download_encrypted").Parse(templates.DownloadHTML)
			if err != nil {
				ob.logf("Error loading template: %v", err)
				http.Error(w, "Error displaying web page, please try refreshing.", http.StatusInternalServerError)
				return
			}
			// Execute template
			if err := t.Execute(w, csrf); err != nil {
				ob.logf("Error executing template: %v", err)
				http.Error(w, "Error displaying web page, please try refreshing.", http.StatusInternalServerError)
				return
			}
		} else {
			if oBuffer.DownloadLimit > 0 && oBuffer.Downloads >= oBuffer.DownloadLimit {
				if err := ob.store.Destroy(oBuffer); err != nil {
					ob.logf("Error deleting onion file from store: %v", err)
				}
				ob.logf("Download limit reached for %s", oBuffer.Name)
				http.Error(w, "Download limit reached.", http.StatusUnauthorized)
				return
			}
			// Validate checksum
			chksmValid, err := oBuffer.ValidateChecksum()
			if err != nil {
				ob.logf("Error validating checksum: %v", err)
				http.Error(w, "Error validating checksum.", http.StatusInternalServerError)
				return
			}
			if !chksmValid {
				ob.logf("Invalid checksum for file %s", oBuffer.Name)
				http.Error(w, "Invalid checksum.", http.StatusInternalServerError)
				return
			}
			// Increment files download count
			oBuffer.Downloads++
			// Check download amount
			if oBuffer.Downloads >= oBuffer.DownloadLimit {
				if err := oBuffer.Destroy(); err != nil {
					ob.logf("Error destroying buffer %s: %v", oBuffer.Name, err)
				}
			}
			// Set headers for browser to initiate download
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.zip", oBuffer.Name))
			// Write the zip bytes to the response for download
			_, err = w.Write(oBuffer.Bytes)
			if err != nil {
				ob.logf("Error writing to client: %v", err)
				http.Error(w, "Error writing to client.", http.StatusInternalServerError)
				return
			}
		}
	// If buffer was password protected
	case http.MethodPost:
		oBuffer := ob.store.Get(r.Header.Get("filename"))
		if oBuffer.DownloadLimit > 0 && oBuffer.Downloads >= oBuffer.DownloadLimit {
			if err := ob.store.Destroy(oBuffer); err != nil {
				ob.logf("Error deleting onion file from store: %v", err)
			}
			ob.logf("Download limit reached for %s", oBuffer.Name)
			http.Error(w, "Download limit reached.", http.StatusUnauthorized)
			return
		}
		// Validate checksum
		chksmValid, err := oBuffer.ValidateChecksum()
		if err != nil {
			ob.logf("Error validating checksum: %v", err)
			http.Error(w, "Error validating checksum.", http.StatusInternalServerError)
			return
		}
		if !chksmValid {
			ob.logf("Invalid checksum for file %s", oBuffer.Name)
			http.Error(w, "Invalid checksum.", http.StatusInternalServerError)
			return
		}
		// Get password and decrypt zip for download
		pass := r.FormValue("password")
		decryptedBytes, err := onion_buffer.Decrypt(oBuffer.Bytes, pass)
		if err != nil {
			ob.logf("Error decrypting buffer: %v", err)
			http.Error(w, "Error decrypting buffer.", http.StatusInternalServerError)
			return
		}
		// Lock memory allotted to decryptedBytes from being used in SWAP
		if err := syscall.Mlock(decryptedBytes); err != nil {
			ob.logf("Error mlocking allotted memory for decryptedBytes: %v", err)
		}
		// Increment files download count
		oBuffer.Downloads++
		// Check download amount
		if oBuffer.Downloads >= oBuffer.DownloadLimit {
			if err := ob.store.Destroy(oBuffer); err != nil {
				ob.logf("Error destroying buffer %s: %v", oBuffer.Name, err)
			}
		}
		// Set headers for browser to initiate download
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.zip", oBuffer.Name))
		// Write the zip bytes to the response for download
		_, err = w.Write(decryptedBytes)
		if err != nil {
			ob.logf("Error writing to client: %v", err)
			http.Error(w, "Error writing to client.", http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "Invalid HTTP Method.", http.StatusMethodNotAllowed)
		return
	}
}

func (ob *onionbox) writeFilesToBuffers(w *zip.Writer, uploadQueue <-chan *multipart.FileHeader, wg *sync.WaitGroup) error {
	for {
		select {
		case fileHeader := <-uploadQueue:
			// Open uploaded file
			file, err := fileHeader.Open()
			if err != nil {
				return err
			}
			// Create file in zip with same name
			zBuffer, err := w.Create(fileHeader.Filename)
			if err != nil {
				return err
			}
			// Read uploaded file
			if err := ob.writeBytesByChunk(file, zBuffer); err != nil {
				return err
			}
			// Flush zipwriter to write compressed bytes to buffer
			// before moving onto the next file
			if err := w.Flush(); err != nil {
				return err
			}
		default:
			if len(uploadQueue) == 0 {
				wg.Done()
			}
		}
	}
}

func (ob *onionbox) writeBytesByChunk(file io.Reader, bufWriter io.Writer) error {
	// Read uploaded file
	var count int
	var err error
	reader := bufio.NewReader(file)
	chunk := make([]byte, ob.chunkSize)
	// Lock memory allotted to chunk from being used in SWAP
	if err := syscall.Mlock(chunk); err != nil {
		return err
	}
	for {
		if count, err = reader.Read(chunk); err != nil {
			break
		}
		_, err := bufWriter.Write(chunk[:count])
		if err != nil {
			return err
		}
	}
	if err != io.EOF {
		return err
	} else {
		err = nil
	}
	return nil
}

// createCSRF creates a simple md5 hash which I use to avoid CSRF attacks when presenting HTML
func createCSRF() (string, error) {
	hasher := md5.New()
	_, err := io.WriteString(hasher, strconv.FormatInt(time.Now().Unix(), 10))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

// logf is a helper function which will utilize the logger from ob
// to print formatted logs.
func (ob *onionbox) logf(format string, args ...interface{}) {
	ob.logger.Printf(format, args...)
}

// quit will quit all stored buffers and exit onionbox.
func (ob *onionbox) quit() {
	if err := ob.store.DestroyAll(); err != nil {
		ob.logf("Error destroying all buffers from store: %v", err)
	}
	os.Exit(0)
}
