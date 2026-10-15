package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pkoukk/tiktoken-go"
)

const maxTokenVocabularyBytes = 8 << 20

func init() {
	// Install before any encoder is initialized; the dependency's default
	// loader uses an HTTP request without a deadline.
	tiktoken.SetBpeLoader(&boundedTokenLoader{client: &http.Client{Timeout: 5 * time.Second}})
}

type boundedTokenLoader struct{ client *http.Client }

func tokenVocabularyCachePath(source string) string {
	dir := strings.TrimSpace(os.Getenv("TIKTOKEN_CACHE_DIR"))
	if dir == "" {
		dir = strings.TrimSpace(os.Getenv("DATA_GYM_CACHE_DIR"))
	}
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "data-gym-cache")
	}
	return filepath.Join(dir, fmt.Sprintf("%x", sha1.Sum([]byte(source))))
}

func (loader *boundedTokenLoader) LoadTiktokenBpe(source string) (map[string]int, error) {
	path := tokenVocabularyCachePath(source)
	if file, err := os.Open(path); err == nil {
		body, readErr := io.ReadAll(io.LimitReader(file, maxTokenVocabularyBytes+1))
		file.Close()
		if readErr == nil {
			if ranks, err := parseTokenVocabulary(body); err == nil {
				return ranks, nil
			}
		}
	}
	response, err := loader.client.Get(source)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token vocabulary returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTokenVocabularyBytes+1))
	if err != nil {
		return nil, err
	}
	ranks, err := parseTokenVocabulary(body)
	if err != nil {
		return nil, err
	}
	// Cache failures do not invalidate a vocabulary that was already fetched
	// and checked. Atomic replacement keeps concurrent processes from reading
	// an incomplete download, using the dependency's existing cache filenames.
	if os.MkdirAll(filepath.Dir(path), 0700) == nil {
		if file, err := os.CreateTemp(filepath.Dir(path), ".tokens-*"); err == nil {
			defer os.Remove(file.Name())
			_, writeErr := file.Write(body)
			closeErr := file.Close()
			if writeErr == nil && closeErr == nil {
				_ = os.Rename(file.Name(), path)
			}
		}
	}
	return ranks, nil
}

func parseTokenVocabulary(body []byte) (map[string]int, error) {
	if len(body) == 0 || len(body) > maxTokenVocabularyBytes {
		return nil, fmt.Errorf("invalid token vocabulary size")
	}
	ranks := make(map[string]int)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid token vocabulary row")
		}
		token, err := base64.StdEncoding.DecodeString(fields[0])
		if err != nil || len(token) == 0 {
			return nil, fmt.Errorf("invalid token vocabulary token")
		}
		rank, err := strconv.Atoi(fields[1])
		if err != nil || rank < 0 {
			return nil, fmt.Errorf("invalid token vocabulary rank")
		}
		if _, duplicate := ranks[string(token)]; duplicate {
			return nil, fmt.Errorf("duplicate token vocabulary token")
		}
		ranks[string(token)] = rank
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(ranks) == 0 {
		return nil, fmt.Errorf("empty token vocabulary")
	}
	return ranks, nil
}
