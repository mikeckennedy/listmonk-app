// Package subimporter implements a bulk ZIP/CSV importer of subscribers.
// It implements a simple queue for buffering imports and committing records
// to DB along with ZIP and CSV handling utilities. It is meant to be used as
// a singleton as each Importer instance is stateful, where it keeps track of
// an import in progress. Only one import should happen on a single importer
// instance at a time.
package subimporter

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/lib/pq"
	uuid "github.com/satori/go.uuid"

	"github.com/asaskevich/govalidator"
	"github.com/knadh/listmonk/models"
)

const (
	// stdInputMaxLen is the maximum allowed length for a standard input field.
	stdInputMaxLen = 200

	// commitBatchSize is the number of inserts to commit in a single SQL transaction.
	commitBatchSize = 10000

	// SubscriberStatusEnabled indicates that a subscriber is active.
	SubscriberStatusEnabled = "enabled"
	// SubscriberStatusDisabled indicates that a subscriber is inactive or unsubscribed.
	SubscriberStatusDisabled = "disabled"
	// SubscriberStatusBlacklisted indicates that a subscriber is blacklisted.
	SubscriberStatusBlacklisted = "blacklisted"

	StatusNone      = "none"
	StatusImporting = "importing"
	StatusStopping  = "stopping"
	StatusFinished  = "finished"
	StatusFailed    = "failed"
)

// Importer represents the bulk CSV subscriber import system.
type Importer struct {
	stmt        *sql.Stmt
	db          *sql.DB
	isImporting bool
	stop        chan bool

	status *Status
	sync.RWMutex
}

// Session represents a single import session.
type Session struct {
	im       *Importer
	subQueue chan models.Subscriber
	log      *log.Logger

	overrideStatus bool
	listIDs        []int
}

// Status reporesents statistics from an ongoing import session.
type Status struct {
	Name     string `json:"name"`
	Total    int    `json:"total"`
	Imported int    `json:"imported"`
	Status   string `json:"status"`
	logBuf   *bytes.Buffer
}

// SubReq is a wrapper over the Subscriber model.
type SubReq struct {
	models.Subscriber
	Lists pq.Int64Array `json:"lists"`
}

var (
	// ErrIsImporting is thrown when an import request is made while an
	// import is already running.
	ErrIsImporting = errors.New("import is already running")

	csvHeaders = map[string]bool{"email": true,
		"name":       true,
		"status":     true,
		"attributes": true}
)

// New returns a new instance of Importer.
func New(stmt *sql.Stmt, db *sql.DB) *Importer {
	im := Importer{
		stmt:   stmt,
		stop:   make(chan bool, 1),
		db:     db,
		status: &Status{Status: StatusNone, logBuf: bytes.NewBuffer(nil)},
	}

	return &im
}

// NewSession returns an new instance of Session. It takes the name
// of the uploaded file, but doesn't do anything with it but retains it for stats.
func (im *Importer) NewSession(fName string, overrideStatus bool, listIDs []int) (*Session, error) {
	if im.getStatus() != StatusNone {
		return nil, errors.New("an import is already running")
	}

	im.Lock()
	im.status = &Status{Status: StatusImporting,
		Name:   fName,
		logBuf: bytes.NewBuffer(nil)}
	im.Unlock()

	s := &Session{
		im:             im,
		log:            log.New(im.status.logBuf, "", log.Ldate|log.Ltime),
		subQueue:       make(chan models.Subscriber, commitBatchSize),
		overrideStatus: overrideStatus,
		listIDs:        listIDs,
	}

	s.log.Printf("processing '%s'", fName)
	return s, nil
}

// GetStats returns the global Stats of the importer.
func (im *Importer) GetStats() Status {
	im.RLock()
	defer im.RUnlock()

	return Status{
		Name:     im.status.Name,
		Status:   im.status.Status,
		Total:    im.status.Total,
		Imported: im.status.Imported,
	}
}

// GetLogs returns the log entries of the last import session.
func (im *Importer) GetLogs() []byte {
	im.RLock()
	defer im.RUnlock()

	if im.status.logBuf == nil {
		return []byte{}
	}

	return im.status.logBuf.Bytes()
}

// setStatus sets the Importer's status.
func (im *Importer) setStatus(status string) {
	im.Lock()
	im.status.Status = status
	im.Unlock()
}

// setStatus get's the Importer's status.
func (im *Importer) getStatus() string {
	im.RLock()
	status := im.status.Status
	im.RUnlock()

	return status
}

// incrementImportCount sets the Importer's "imported" counter.
func (im *Importer) incrementImportCount(n int) {
	im.Lock()
	im.status.Imported += n
	im.Unlock()
}

// Start is a blocking function that selects on a channel queue until all
// subscriber entries in the import session are imported. It should be
// invoked as a goroutine.
func (s *Session) Start() {
	var (
		tx    *sql.Tx
		stmt  *sql.Stmt
		err   error
		total = 0
		cur   = 0

		listIDs = make(pq.Int64Array, len(s.listIDs))
	)

	for i, v := range s.listIDs {
		listIDs[i] = int64(v)
	}

	for sub := range s.subQueue {
		if cur == 0 {
			// New transaction batch.
			tx, err = s.im.db.Begin()
			if err != nil {
				s.log.Printf("error creating DB transaction: %v", err)
				continue
			}
			stmt = tx.Stmt(s.im.stmt)
		}

		_, err := stmt.Exec(
			uuid.NewV4(),
			sub.Email,
			sub.Name,
			sub.Status,
			sub.Attribs,
			s.overrideStatus,
			listIDs)
		if err != nil {
			s.log.Printf("error executing insert: %v", err)
			continue
		}
		cur++
		total++

		// Batch size is met. Commit.
		if cur%commitBatchSize == 0 {
			if err := tx.Commit(); err != nil {
				tx.Rollback()
				s.log.Printf("error committing to DB: %v", err)
			} else {
				s.im.incrementImportCount(cur)
				s.log.Printf("imported %d", total)
			}

			cur = 0
		}
	}

	// Queue's closed and there's nothing left to commit.
	if cur == 0 {
		s.im.setStatus(StatusFinished)
		s.log.Printf("imported finished")
		return
	}

	// Queue's closed and there are records left to commit.
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		s.im.setStatus(StatusFailed)
		s.log.Printf("error committing to DB: %v", err)
		return
	}

	s.im.incrementImportCount(cur)
	s.im.setStatus(StatusFinished)
	s.log.Printf("imported finished")
}

// ExtractZIP takes a ZIP file's path and extracts all .csv files in it to
// a temporary directory, and returns the name of the temp directory and the
// list of extracted .csv files.
func (s *Session) ExtractZIP(srcPath string, maxCSVs int) (string, []string, error) {
	if s.im.isImporting {
		return "", nil, ErrIsImporting
	}

	z, err := zip.OpenReader(srcPath)
	if err != nil {
		return "", nil, err
	}
	defer z.Close()

	// Create a temporary directory to extract the files.
	dir, err := ioutil.TempDir("", "listmonk")
	if err != nil {
		s.log.Printf("error creating temporary directory for extracting ZIP: %v", err)
		return "", nil, err
	}

	var files []string
	for _, f := range z.File {
		fName := f.FileInfo().Name()

		// Skip directories.
		if f.FileInfo().IsDir() {
			s.log.Printf("skipping directory '%s'", fName)
			continue
		}

		// Skip files without the .csv extension.
		if !strings.Contains(strings.ToLower(fName), "csv") {
			s.log.Printf("skipping non .csv file '%s'", fName)
			continue
		}

		s.log.Printf("extracting '%s'", fName)
		src, err := f.Open()
		if err != nil {
			s.log.Printf("error opening '%s' from ZIP: '%v'", fName, err)
			return "", nil, err
		}
		defer src.Close()

		out, err := os.OpenFile(dir+"/"+fName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			s.log.Printf("error creating '%s/%s': '%v'", dir, fName, err)
			return "", nil, err
		}
		defer out.Close()

		if _, err := io.Copy(out, src); err != nil {
			s.log.Printf("error extracting to '%s/%s': '%v'", dir, fName, err)
			return "", nil, err
		}
		s.log.Printf("extracted '%s'", fName)

		files = append(files, fName)
		if len(files) > maxCSVs {
			s.log.Printf("won't extract any more files. Maximum is %d", maxCSVs)
			break
		}
	}

	return dir, files, nil
}

// LoadCSV loads a CSV file and validates and imports the subscriber entries in it.
func (s *Session) LoadCSV(srcPath string, delim rune) error {
	if s.im.isImporting {
		return ErrIsImporting
	}

	// Default status is "failed" in case the function
	// returns at one of the many possible errors.
	failed := true
	defer func() {
		if failed {
			s.im.setStatus(StatusFailed)
		}
	}()

	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}

	// Count the total number of lines in the file. This doesn't distinguish
	// between "blank" and non "blank" lines, and is only used to derive
	// the progress percentage for the frontend.
	numLines, err := countLines(f)
	if err != nil {
		s.log.Printf("error counting lines in '%s': '%v'", srcPath, err)
		return err
	}
	s.im.Lock()
	s.im.status.Total = numLines
	s.im.Unlock()

	// Rewind, now that we've done a linecount on the same handler.
	f.Seek(0, 0)
	rd := csv.NewReader(f)
	rd.Comma = delim

	// Read the header.
	csvHdr, err := rd.Read()
	if err != nil {
		s.log.Printf("error reading header from '%s': '%v'", srcPath, err)
		return err
	}

	hdrKeys := s.mapCSVHeaders(csvHdr, csvHeaders)
	// email, and name are required headers.
	if _, ok := hdrKeys["email"]; !ok {
		s.log.Printf("'email' column not found in '%s'", srcPath)
		return errors.New("'email' column not found")
	}
	if _, ok := hdrKeys["name"]; !ok {
		s.log.Printf("'name' column not found in '%s'", srcPath)
		return errors.New("'name' column not found")
	}

	var (
		lnHdr = len(hdrKeys)
		i     = 1
	)
	for {
		// Check for the stop signal.
		select {
		case <-s.im.stop:
			failed = false
			close(s.subQueue)
			s.log.Println("stop request received")
			return nil
		default:
		}

		cols, err := rd.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			s.log.Printf("error reading CSV '%s'", err)
			return err
		}

		lnCols := len(cols)
		if lnCols < lnHdr {
			s.log.Printf("skipping line %d. column count (%d) does not match minimum header count (%d)", i, lnCols, lnHdr)
			continue
		}

		// Iterate the key map and based on the indices mapped earlier,
		// form a map of key: csv_value, eg: email: user@user.com.
		row := make(map[string]string, lnCols)
		for key := range csvHeaders {
			row[key] = cols[hdrKeys[key]]
		}

		// Lowercase to ensure uniqueness in the DB.
		row["email"] = strings.ToLower(strings.TrimSpace(row["email"]))

		sub := models.Subscriber{
			Email:  row["email"],
			Name:   row["name"],
			Status: row["status"],
		}

		// JSON attributes.
		if len(row["attributes"]) > 0 {
			var (
				attribs models.SubscriberAttribs
				b       = []byte(row["attributes"])
			)
			if err := json.Unmarshal(b, &attribs); err != nil {
				s.log.Printf("skipping invalid attributes JSON on line %d for '%s': %v", i, sub.Email, err)
			} else {
				sub.Attribs = attribs
			}
		}

		// Send the subscriber to the queue.
		s.subQueue <- sub

		i++
	}

	close(s.subQueue)
	failed = false

	return nil
}

// Stop sends a signal to stop all existing imports.
func (im *Importer) Stop() {
	if im.getStatus() != StatusImporting {
		im.Lock()
		im.status = &Status{Status: StatusNone}
		im.Unlock()
		return
	}

	select {
	case im.stop <- true:
		im.setStatus(StatusStopping)
	default:
	}
}

// mapCSVHeaders takes a list of headers obtained from a CSV file, a map of known headers,
// and returns a new map with each of the headers in the known map mapped by the position (0-n)
// in the given CSV list.
func (s *Session) mapCSVHeaders(csvHdrs []string, knownHdrs map[string]bool) map[string]int {
	// Map 0-n column index to the header keys, name: 0, email: 1 etc.
	// This is to allow dynamic ordering of columns in th CSV.
	hdrKeys := make(map[string]int)
	for i, h := range csvHdrs {
		if _, ok := knownHdrs[h]; !ok {
			s.log.Printf("ignoring unknown header '%s'", h)
			continue
		}

		hdrKeys[h] = i
	}

	return hdrKeys
}

// ValidateFields validates incoming subscriber field values.
func ValidateFields(s SubReq) error {
	if !govalidator.IsEmail(s.Email) {
		return errors.New("invalid `email`")
	}
	if !govalidator.IsByteLength(s.Name, 1, stdInputMaxLen) {
		return errors.New("invalid length for `name`")
	}
	if s.Status != SubscriberStatusEnabled && s.Status != SubscriberStatusDisabled &&
		s.Status != SubscriberStatusBlacklisted {
		return errors.New("invalid `status`")
	}

	return nil
}

// countLines counts the number of line breaks in a file. This does not
// distinguish between "blank" and non "blank" lines.
// Credit: https://stackoverflow.com/a/24563853
func countLines(r io.Reader) (int, error) {
	var (
		buf     = make([]byte, 32*1024)
		count   = 0
		lineSep = []byte{'\n'}
	)

	for {
		c, err := r.Read(buf)
		count += bytes.Count(buf[:c], lineSep)

		switch {
		case err == io.EOF:
			return count, nil

		case err != nil:
			return count, err
		}
	}

}