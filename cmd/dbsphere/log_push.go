package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cuipengdba/dbsphere/internal/loganalysis"
	"github.com/cuipengdba/dbsphere/internal/store"
)

func pushLog(args []string) error {
	flags := flag.NewFlagSet("push-log", flag.ContinueOnError)
	filePath := flags.String("file", "", "path to the source log")
	generation := flags.Int64("generation", 0, "source file generation, starting at 1")
	final := flags.Bool("final", false, "the selected log file is closed and ready to seal")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *filePath == "" || *generation < 1 || flags.NArg() != 0 {
		return errors.New("specify -file and -generation")
	}
	endpoint, err := url.Parse(os.Getenv("DBSPHERE_LOG_SOURCE_URL"))
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		(endpoint.Scheme != "https" && (endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" && endpoint.Hostname() != "::1" && endpoint.Hostname() != "localhost")) {
		return errors.New("set DBSPHERE_LOG_SOURCE_URL to the HTTPS source endpoint or a local test endpoint")
	}
	token := os.Getenv("DBSPHERE_LOG_SOURCE_TOKEN")
	if decoded, err := hex.DecodeString(token); err != nil || len(decoded) != 32 {
		return errors.New("set DBSPHERE_LOG_SOURCE_TOKEN to the source credential")
	}
	file, err := os.Open(*filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("the selected log must be a regular file")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	request := func(method string, chunk store.LogStreamChunk) (store.LogStreamCursor, error) {
		var cursor store.LogStreamCursor
		req, err := http.NewRequest(method, endpoint.String(), bytes.NewReader(chunk.Raw))
		if err != nil {
			return cursor, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if method == http.MethodPut {
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("X-Log-Generation", strconv.FormatInt(chunk.Generation, 10))
			req.Header.Set("X-Log-Offset", strconv.FormatInt(chunk.Offset, 10))
			req.Header.Set("X-Log-Size", strconv.FormatInt(chunk.SenderSize, 10))
			req.Header.Set("X-Log-Filename", chunk.Filename)
			req.Header.Set("X-Log-Final", strconv.FormatBool(chunk.Final))
		}
		resp, err := client.Do(req)
		if err != nil {
			return cursor, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			return cursor, fmt.Errorf("log source returned HTTP %d", resp.StatusCode)
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&cursor)
		return cursor, err
	}
	cursor, err := request(http.MethodGet, store.LogStreamChunk{})
	if err != nil {
		return err
	}
	if cursor.Generation < 1 || cursor.NextOffset < 0 || cursor.Format == "" {
		return errors.New("invalid log source cursor")
	}
	if *generation < cursor.Generation || *generation > cursor.Generation && (!cursor.Final || *generation-cursor.Generation != 1) {
		return errors.New("generation differs from source cursor")
	}
	name := filepath.Base(*filePath)
	if *generation == cursor.Generation && cursor.Filename != "" {
		name = cursor.Filename
	}
	if *generation == cursor.Generation && cursor.Final {
		if cursor.NextOffset != info.Size() {
			return errors.New("sealed generation length differs from selected file")
		}
		return json.NewEncoder(os.Stdout).Encode(cursor)
	}
	offset := cursor.NextOffset
	if *generation != cursor.Generation {
		offset = 0
	}
	if offset > info.Size() {
		return errors.New("selected file is shorter than the acknowledged offset")
	}
	buffer := make([]byte, store.LogArchiveBlockBytes)
	for {
		length := min(int64(len(buffer)), info.Size()-offset)
		if length != 0 {
			if _, err := file.ReadAt(buffer[:length], offset); err != nil {
				return err
			}
		}
		sealed := *final && offset+length == info.Size()
		if !sealed {
			safe, err := loganalysis.CompleteStreamPrefix(buffer[:length], cursor.Format, cursor.Prefix)
			if err != nil {
				return err
			}
			if safe == 0 {
				if length == int64(len(buffer)) {
					return errors.New("no complete record boundary in the 1 MiB source buffer")
				}
				break
			}
			length = int64(safe)
		}
		ack, err := request(http.MethodPut, store.LogStreamChunk{Generation: *generation, Offset: offset,
			SenderSize: info.Size(), Filename: name, Final: sealed, Raw: buffer[:length]})
		if err != nil {
			return err
		}
		if ack.Generation != *generation || ack.NextOffset != offset+length || ack.Final != sealed {
			return errors.New("log source acknowledged a different cursor")
		}
		offset = ack.NextOffset
		cursor.Generation, cursor.NextOffset, cursor.Filename, cursor.Final = ack.Generation, offset, ack.Filename, ack.Final
		if sealed {
			break
		}
	}
	return json.NewEncoder(os.Stdout).Encode(cursor)
}
