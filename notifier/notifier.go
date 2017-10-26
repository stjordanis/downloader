package notifier

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"golang.skroutz.gr/skroutz/downloader/job"
	"golang.skroutz.gr/skroutz/downloader/storage"
)

const maxCallbackRetries = 2

// CallbackInfo holds info to be posted back to the provided callback url.
type CallbackInfo struct {
	Success     bool   `json:"success"`
	Error       string `json:"error"`
	Extra       string `json:"extra"`
	DownloadURL string `json:"download_url"`
}

// Notifier is the the component responsible for consuming the result of jobs
// and notifying back the respective users by issuing HTTP requests to their
// provided callback URLs.
type Notifier struct {
	Storage     *storage.Storage
	Log         *log.Logger
	DownloadURL *url.URL

	// TODO: These should be exported
	concurrency int
	client      *http.Client
	cbChan      chan job.Job
}

// NewNotifier takes the concurrency of the notifier as an argument
func New(s *storage.Storage, concurrency int, logger *log.Logger, dwnlURL string) (Notifier, error) {
	url, err := url.ParseRequestURI(dwnlURL)
	if err != nil {
		return Notifier{}, fmt.Errorf("Could not parse Download URL, %v", err)
	}

	if concurrency <= 0 {
		return Notifier{}, errors.New("Notifier Concurrency must be a positive number")
	}

	return Notifier{
		Storage:     s,
		Log:         logger,
		concurrency: concurrency,
		client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{},
			},
			Timeout: time.Duration(3) * time.Second,
		},
		cbChan:      make(chan job.Job),
		DownloadURL: url,
	}, nil
}

// Start starts the Notifier loop and instruments the worker goroutines that
// perform the actual notify requests.
func (n *Notifier) Start(closeChan chan struct{}) {
	var wg sync.WaitGroup
	wg.Add(n.concurrency)
	for i := 0; i < n.concurrency; i++ {
		go func() {
			defer wg.Done()
			for job := range n.cbChan {
				err := n.Notify(&job)
				if err != nil {
					n.Log.Printf("Notify error: %s", err)
				}
			}

		}()
	}

	// Check Redis for jobs left in InProgress state
	n.collectRogueCallbacks()

	for {
		select {
		case <-closeChan:
			close(n.cbChan)
			wg.Wait()
			closeChan <- struct{}{}
			return
		default:
			job, err := n.Storage.PopCallback()
			if err != nil {
				switch err {
				case storage.ErrEmptyQueue:
					// noop
				case storage.ErrRetryLater:
					// noop
				default:
					n.Log.Println(err)
				}

				time.Sleep(time.Second)
				continue
			}
			n.cbChan <- job
		}
	}
}

// collectRogueCallbacks Scans Redis for jobs that have InProgress CallbackState.
// This indicates they are leftover from an interrupted previous run and should get requeued.
func (n *Notifier) collectRogueCallbacks() {
	var cursor uint64
	var rogueCount uint64

	for {
		var keys []string
		var err error
		keys, cursor, err = n.Storage.Redis.Scan(cursor, storage.JobKeyPrefix+"*", 50).Result()
		if err != nil {
			n.Log.Println(err)
			break
		}

		for _, jobID := range keys {
			strCmd := n.Storage.Redis.HGet(jobID, "CallbackState")
			if strCmd.Err() != nil {
				n.Log.Println(strCmd.Err())
				continue
			}
			if job.State(strCmd.Val()) == job.StateInProgress {
				jb, err := n.Storage.GetJob(strings.TrimPrefix(jobID, storage.JobKeyPrefix))
				if err != nil {
					n.Log.Printf("Could not get job for Redis: %v", err)
					continue
				}
				err = n.Storage.QueuePendingCallback(&jb)
				if err != nil {
					n.Log.Printf("Could not queue job for download: %v", err)
					continue
				}
				rogueCount++
			}
		}

		if cursor == 0 {
			break
		}
	}

	if rogueCount > 0 {
		n.Log.Printf("Queued %d rogue callbacks", rogueCount)
	}
}

// Notify posts callback info to j.CallbackURL
func (n *Notifier) Notify(j *job.Job) error {
	j.CallbackCount++

	err := n.markCbInProgress(j)
	if err != nil {
		return err
	}

	cbInfo, err := n.getCallbackInfo(j)
	if err != nil {
		return n.markCbFailed(j, err.Error())
	}

	cb, err := json.Marshal(cbInfo)
	if err != nil {
		return n.markCbFailed(j, err.Error())
	}

	res, err := n.client.Post(j.CallbackURL, "application/json", bytes.NewBuffer(cb))
	if err != nil || res.StatusCode < 200 || res.StatusCode >= 300 {
		if err == nil {
			err = fmt.Errorf("Received Status: %s", res.Status)
		}
		return n.retryOrFail(j, err.Error())
	}

	return n.Storage.RemoveJob(j.ID)
}

// retryOrFail checks the callback count of the current download
// and retries the callback if its Retry Counts < maxRetries else it marks
// it as failed
func (n *Notifier) retryOrFail(j *job.Job, err string) error {
	if j.CallbackCount >= maxCallbackRetries {
		return n.markCbFailed(j, err)
	}
	return n.Storage.QueuePendingCallback(j)
}

// callbackInfo validates that the job is good for callback and
// return callbackInfo to the caller
func (n *Notifier) getCallbackInfo(j *job.Job) (CallbackInfo, error) {
	if j.DownloadState != job.StateSuccess && j.DownloadState != job.StateFailed {
		return CallbackInfo{}, fmt.Errorf("Invalid job download state: '%s'", j.DownloadState)
	}

	return CallbackInfo{
		Success:     j.DownloadState == job.StateSuccess,
		Error:       j.DownloadMeta,
		Extra:       j.Extra,
		DownloadURL: jobDownloadURL(j, *n.DownloadURL),
	}, nil
}

// jobdownloadURL constructs the actual download URL to be provided to the user.
func jobDownloadURL(j *job.Job, downloadURL url.URL) string {
	if j.DownloadState != job.StateSuccess {
		return ""
	}

	downloadURL.Path = path.Join(downloadURL.Path, j.ID)
	return downloadURL.String()
}

func (n *Notifier) markCbInProgress(j *job.Job) error {
	j.CallbackState = job.StateInProgress
	j.CallbackMeta = ""
	return n.Storage.SaveJob(j)
}

func (n *Notifier) markCbFailed(j *job.Job, meta ...string) error {
	j.CallbackState = job.StateFailed
	j.CallbackMeta = strings.Join(meta, "\n")
	n.Log.Printf("Callback failed: {%s, %s}, destination %s", j.ID, j.AggrID, j.CallbackURL)
	return n.Storage.SaveJob(j)
}
