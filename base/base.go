// Copyright 2016, Google
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package base provides a very low-level interface on top of the B2 v1 API.
// It is not intended to be used directly.
//
// It currently lacks support for the following APIs:
//
// b2_download_file_by_id
// b2_get_file_info
// b2_hide_file
// b2_list_parts
// b2_list_unfinished_large_files
// b2_update_bucket
package base

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/context"
)

const (
	apiBase = "https://api.backblaze.com"
	apiV1   = "/b2api/v1/"
)

type b2err struct {
	msg    string
	method string
	retry  int
	code   int
}

func (e b2err) Error() string {
	if e.method == "" {
		return fmt.Sprintf("b2 error: %s", e.msg)
	}
	return fmt.Sprintf("%s: %d: %s", e.method, e.code, e.msg)
}

// Action checks an error and returns a recommended course of action.
func Action(err error) ErrAction {
	e, ok := err.(b2err)
	if !ok {
		return Punt
	}
	if e.retry > 0 {
		return Retry
	}
	if e.code >= 500 && e.code < 600 {
		if e.method == "b2_upload_file" || e.method == "b2_upload_part" {
			return AttemptNewUpload
		}
	}
	switch e.code {
	case 401:
		if e.method == "b2_authorize_account" {
			return Punt
		}
		return ReAuthenticate
	case 429, 500, 503:
		return Retry
	}
	return Punt
}

// ErrAction is an action that a caller can take when any function returns an
// error.
type ErrAction int

const (
	// ReAuthenticate indicates that the B2 account authentication tokens have
	// expired, and should be refreshed with a new call to AuthorizeAccount.
	ReAuthenticate ErrAction = iota

	// AttemptNewUpload indicates that an upload's authentication token (or URL
	// endpoint) has expored, and that users should request new ones with a call
	// to GetUploadURL or GetUploadPartURL.
	AttemptNewUpload

	// Retry indicates that the caller should wait an appropriate amount of time,
	// and then reattempt the RPC.
	Retry

	// Punt means that there is no useful action to be taken on this error, and
	// that it should be displayed to the user.
	Punt
)

type errMsg struct {
	Msg string `json:"message"`
}

func mkErr(resp *http.Response) error {
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	msg := &errMsg{}
	if err := json.Unmarshal(data, msg); err != nil {
		return err
	}
	var retryAfter int
	retry := resp.Header.Get("Retry-After")
	if retry != "" {
		r, err := strconv.ParseInt(retry, 10, 64)
		if err != nil {
			return err
		}
		retryAfter = int(r)
	}
	return b2err{
		msg:    msg.Msg,
		retry:  retryAfter,
		code:   resp.StatusCode,
		method: resp.Request.Header.Get("X-Blazer-Method"),
	}
}

// Backoff returns an appropriate amount of time to wait, given an error, if
// any was returned by the server.  If the return value is 0, but Action
// indicates Retry, the user should implement their own exponential backoff,
// beginning with one second.
func Backoff(err error) time.Duration {
	e, ok := err.(b2err)
	if !ok {
		return 0
	}
	return time.Duration(e.retry) * time.Second
}

var (
	logger *log.Logger = nil
)

func SetTraceOutput(w io.Writer) {
	logger = log.New(w, "b2_trace", log.Ldate|log.Ltime|log.Lshortfile)
}

func logRequest(req *http.Request, args []byte) {
	if logger == nil {
		return
	}
	var headers []string
	for k, v := range req.Header {
		if k == "Authorization" || k == "X-Blazer-Method" {
			continue
		}
		headers = append(headers, fmt.Sprintf("%s: %s", k, strings.Join(v, ",")))
	}
	hstr := strings.Join(headers, ";")
	method := req.Header.Get("X-Blazer-Method")
	if args != nil {
		logger.Printf(">> %s uri: %v headers: {%s} args: (%s)", method, req.URL, hstr, string(args))
		return
	}
	logger.Printf(">> %s uri: %v {%s} (no args)", method, req.URL, hstr)
}

func logResponse(resp *http.Response, reply []byte) {
	if logger == nil {
		return
	}
	var headers []string
	for k, v := range resp.Header {
		headers = append(headers, fmt.Sprintf("%s: %s", k, strings.Join(v, ",")))
	}
	hstr := strings.Join(headers, ";")
	method := resp.Request.Header.Get("X-Blazer-Method")
	id := resp.Request.Header.Get("X-Blazer-Request-ID")
	if reply != nil {
		logger.Printf("<< %s (%s) %s {%s} (%s)", method, id, resp.Status, hstr, string(reply))
		return
	}
	logger.Printf("<< %s (%s) %s {%s} (no reply)", method, id, resp.Status, hstr)
}

// B2 holds account information for Backblaze.
type B2 struct {
	accountID   string
	authToken   string
	apiURI      string
	downloadURI string
	minPartSize int
}

// Update replaces the B2 object with a new one, in-place.
func (b *B2) Update(n *B2) {
	b.accountID = n.accountID
	b.authToken = n.authToken
	b.apiURI = n.apiURI
	b.downloadURI = n.downloadURI
	b.minPartSize = n.minPartSize
}

type b2AuthorizeAccountResponse struct {
	AccountID   string `json:"accountId"`
	AuthToken   string `json:"authorizationToken"`
	URI         string `json:"apiUrl"`
	DownloadURI string `json:"downloadUrl"`
	MinPartSize int    `json:"minimumPartSize"`
}

type httpReply struct {
	resp *http.Response
	err  error
}

func makeNetRequest(req *http.Request) <-chan httpReply {
	ch := make(chan httpReply)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		ch <- httpReply{resp, err}
		close(ch)
	}()
	return ch
}

// FailSomeUploads causes B2 to return errors, randomly, to some RPCs.  It is
// intended to be used for integration testing.
var FailSomeUploads = false

var reqID int64

func makeRequest(ctx context.Context, method, verb, url string, b2req, b2resp interface{}, headers map[string]string, body io.Reader) error {
	var args []byte
	if b2req != nil {
		enc, err := json.Marshal(b2req)
		if err != nil {
			return err
		}
		args = enc
		body = bytes.NewBuffer(enc)
	}
	req, err := http.NewRequest(verb, url, body)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Blazer-Request-ID", fmt.Sprintf("%d", atomic.AddInt64(&reqID, 1)))
	req.Header.Set("X-Blazer-Method", method)
	if FailSomeUploads {
		req.Header.Set("X-Bz-Test-Mode", "fail_some_uploads")
	}
	cancel := make(chan struct{})
	req.Cancel = cancel
	logRequest(req, args)
	ch := makeNetRequest(req)
	var reply httpReply
	select {
	case reply = <-ch:
	case <-ctx.Done():
		close(cancel)
		return ctx.Err()
	}
	if reply.err != nil {
		return reply.err
	}
	resp := reply.resp
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		logResponse(resp, nil)
		return mkErr(resp)
	}
	var replyArgs []byte
	if b2resp != nil {
		rbuf := &bytes.Buffer{}
		r := io.TeeReader(resp.Body, rbuf)
		decoder := json.NewDecoder(r)
		if err := decoder.Decode(b2resp); err != nil {
			return err
		}
		replyArgs = rbuf.Bytes()
	}
	logResponse(resp, replyArgs)
	return nil
}

// AuthorizeAccount wraps b2_authorize_account.
func AuthorizeAccount(ctx context.Context, account, key string) (*B2, error) {
	auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", account, key)))
	b2resp := &b2AuthorizeAccountResponse{}
	headers := map[string]string{
		"Authorization": fmt.Sprintf("Basic %s", auth),
	}
	if err := makeRequest(ctx, "b2_authorize_account", "GET", apiBase+apiV1+"b2_authorize_account", nil, b2resp, headers, nil); err != nil {
		return nil, err
	}
	return &B2{
		accountID:   b2resp.AccountID,
		authToken:   b2resp.AuthToken,
		apiURI:      b2resp.URI,
		downloadURI: b2resp.DownloadURI,
		minPartSize: b2resp.MinPartSize,
	}, nil
}

type b2CreateBucketRequest struct {
	AccountID string `json:"accountId"`
	Name      string `json:"bucketName"`
	Type      string `json:"bucketType"`
}

type b2CreateBucketResponse struct {
	BucketID string `json:"bucketId"`
}

// CreateBucket wraps b2_create_bucket.
func (b *B2) CreateBucket(ctx context.Context, name, btype string) (*Bucket, error) {
	if btype != "allPublic" {
		btype = "allPrivate"
	}
	b2req := &b2CreateBucketRequest{
		AccountID: b.accountID,
		Name:      name,
		Type:      btype,
	}
	b2resp := &b2CreateBucketResponse{}
	headers := map[string]string{
		"Authorization": b.authToken,
	}
	if err := makeRequest(ctx, "b2_create_bucket", "POST", b.apiURI+apiV1+"b2_create_bucket", b2req, b2resp, headers, nil); err != nil {
		return nil, err
	}
	return &Bucket{
		Name: name,
		id:   b2resp.BucketID,
		b2:   b,
	}, nil
}

type b2DeleteBucketRequest struct {
	AccountID string `json:"accountId"`
	BucketID  string `json:"bucketId"`
}

// DeleteBucket wraps b2_delete_bucket.
func (b *Bucket) DeleteBucket(ctx context.Context) error {
	b2req := &b2DeleteBucketRequest{
		AccountID: b.b2.accountID,
		BucketID:  b.id,
	}
	headers := map[string]string{
		"Authorization": b.b2.authToken,
	}
	return makeRequest(ctx, "b2_delete_bucket", "POST", b.b2.apiURI+apiV1+"b2_delete_bucket", b2req, nil, headers, nil)
}

// Bucket holds B2 bucket details.
type Bucket struct {
	Name string
	id   string
	b2   *B2
}

type b2ListBucketsRequest struct {
	AccountID string `json:"accountId"`
}

type b2ListBucketsResponse struct {
	Buckets []struct {
		BucketID   string `json:"bucketId"`
		BucketName string `json:"bucketName"`
		BucketType string `json:"bucketType"`
	} `json:"buckets"`
}

// ListBuckets wraps b2_list_buckets.
func (b *B2) ListBuckets(ctx context.Context) ([]*Bucket, error) {
	b2req := &b2ListBucketsRequest{
		AccountID: b.accountID,
	}
	b2resp := &b2ListBucketsResponse{}
	headers := map[string]string{
		"Authorization": b.authToken,
	}
	if err := makeRequest(ctx, "b2_list_buckets", "POST", b.apiURI+apiV1+"b2_list_buckets", b2req, b2resp, headers, nil); err != nil {
		return nil, err
	}
	var buckets []*Bucket
	for _, bucket := range b2resp.Buckets {
		buckets = append(buckets, &Bucket{
			Name: bucket.BucketName,
			id:   bucket.BucketID,
			b2:   b,
		})
	}
	return buckets, nil
}

type b2GetUploadURLRequest struct {
	BucketID string `json:"bucketId"`
}

type b2GetUploadURLResponse struct {
	URI   string `json:"uploadUrl"`
	Token string `json:"authorizationToken"`
}

// URL holds information from the b2_get_upload_url API.
type URL struct {
	uri    string
	token  string
	b2     *B2
	bucket *Bucket
}

// Reload reloads URL in-place, by reissuing a b2_get_upload_url and
// overwriting the previous values.
func (url *URL) Reload(ctx context.Context) error {
	n, err := url.bucket.GetUploadURL(ctx)
	if err != nil {
		return err
	}
	url.uri = n.uri
	url.token = n.token
	return nil
}

// GetUploadURL wraps b2_get_upload_url.
func (b *Bucket) GetUploadURL(ctx context.Context) (*URL, error) {
	b2req := &b2GetUploadURLRequest{
		BucketID: b.id,
	}
	b2resp := &b2GetUploadURLResponse{}
	headers := map[string]string{
		"Authorization": b.b2.authToken,
	}
	if err := makeRequest(ctx, "b2_get_upload_url", "POST", b.b2.apiURI+apiV1+"b2_get_upload_url", b2req, b2resp, headers, nil); err != nil {
		return nil, err
	}
	return &URL{
		uri:    b2resp.URI,
		token:  b2resp.Token,
		b2:     b.b2,
		bucket: b,
	}, nil
}

// File represents a B2 file.
type File struct {
	Name   string
	Size   int64
	Status string
	id     string
	b2     *B2
}

type b2UploadFileResponse struct {
	FileID string `json:"fileId"`
}

// UploadFile wraps b2_upload_file.
func (url *URL) UploadFile(ctx context.Context, r io.Reader, size int, name, contentType, sha1 string, info map[string]string) (*File, error) {
	headers := map[string]string{
		"Authorization":     url.token,
		"X-Bz-File-Name":    name,
		"Content-Type":      contentType,
		"Content-Length":    fmt.Sprintf("%d", size),
		"X-Bz-Content-Sha1": sha1,
	}
	for k, v := range info {
		headers[fmt.Sprintf("X-Bz-Info-%s", k)] = v
	}
	b2resp := &b2UploadFileResponse{}
	if err := makeRequest(ctx, "b2_upload_file", "POST", url.uri, nil, b2resp, headers, r); err != nil {
		return nil, err
	}
	return &File{
		Name: name,
		Size: int64(size),
		id:   b2resp.FileID,
		b2:   url.b2,
	}, nil
}

type b2DeleteFileVersionRequest struct {
	Name   string `json:"fileName"`
	FileID string `json:"fileId"`
}

// DeleteFileVersion wraps b2_delete_file_version.
func (f *File) DeleteFileVersion(ctx context.Context) error {
	b2req := &b2DeleteFileVersionRequest{
		Name:   f.Name,
		FileID: f.id,
	}
	headers := map[string]string{
		"Authorization": f.b2.authToken,
	}
	return makeRequest(ctx, "b2_delete_file_version", "POST", f.b2.apiURI+apiV1+"b2_delete_file_version", b2req, nil, headers, nil)
}

type startLargeFileRequest struct {
	BucketID    string            `json:"bucketId"`
	Name        string            `json:"fileName"`
	ContentType string            `json:"contentType"`
	Info        map[string]string `json:"fileInfo"`
}

type startLargeFileResponse struct {
	ID string `json:"fileId"`
}

// LargeFile holds information necessary to implement B2 large file support.
type LargeFile struct {
	id string
	b2 *B2

	mu     sync.Mutex
	size   int64
	hashes map[int]string
}

// StartLargeFile wraps b2_start_large_file.
func (b *Bucket) StartLargeFile(ctx context.Context, name, contentType string, info map[string]string) (*LargeFile, error) {
	b2req := &startLargeFileRequest{
		BucketID:    b.id,
		Name:        name,
		ContentType: contentType,
		Info:        info,
	}
	b2resp := &startLargeFileResponse{}
	headers := map[string]string{
		"Authorization": b.b2.authToken,
	}
	if err := makeRequest(ctx, "b2_start_large_file", "POST", b.b2.apiURI+apiV1+"b2_start_large_file", b2req, b2resp, headers, nil); err != nil {
		return nil, err
	}
	return &LargeFile{
		id:     b2resp.ID,
		b2:     b.b2,
		hashes: make(map[int]string),
	}, nil
}

type cancelLargeFileRequest struct {
	ID string `json:"fileId"`
}

// CancelLargeFile wraps b2_cancel_large_file.
func (l *LargeFile) CancelLargeFile(ctx context.Context) error {
	b2req := &cancelLargeFileRequest{
		ID: l.id,
	}
	headers := map[string]string{
		"Authorization": l.b2.authToken,
	}
	if err := makeRequest(ctx, "b2_cancel_large_file", "POST", l.b2.apiURI+apiV1+"b2_cancel_large_file", b2req, nil, headers, nil); err != nil {
		return err
	}
	return nil
}

// FileChunk holds information necessary for uploading file chunks.
type FileChunk struct {
	url   string
	token string
	file  *LargeFile
}

type getUploadPartURLRequest struct {
	ID string `json:"fileId"`
}

type getUploadPartURLResponse struct {
	URL   string `json:"uploadUrl"`
	Token string `json:"authorizationToken"`
}

// GetUploadPartURL wraps b2_get_upload_part_url.
func (l *LargeFile) GetUploadPartURL(ctx context.Context) (*FileChunk, error) {
	b2req := &getUploadPartURLRequest{
		ID: l.id,
	}
	b2resp := &getUploadPartURLResponse{}
	headers := map[string]string{
		"Authorization": l.b2.authToken,
	}
	if err := makeRequest(ctx, "b2_get_upload_part_url", "POST", l.b2.apiURI+apiV1+"b2_get_upload_part_url", b2req, b2resp, headers, nil); err != nil {
		return nil, err
	}
	return &FileChunk{
		url:   b2resp.URL,
		token: b2resp.Token,
		file:  l,
	}, nil
}

// Reload reloads FileChunk in-place.
func (fc *FileChunk) Reload(ctx context.Context) error {
	n, err := fc.file.GetUploadPartURL(ctx)
	if err != nil {
		return err
	}
	fc.url = n.url
	fc.token = n.token
	return nil
}

// UploadPart wraps b2_upload_part.
func (fc *FileChunk) UploadPart(ctx context.Context, r io.Reader, sha1 string, size, index int) (int, error) {
	headers := map[string]string{
		"Authorization":     fc.token,
		"X-Bz-Part-Number":  fmt.Sprintf("%d", index),
		"Content-Length":    fmt.Sprintf("%d", size),
		"X-Bz-Content-Sha1": sha1,
	}
	if err := makeRequest(ctx, "b2_upload_part", "POST", fc.url, nil, nil, headers, r); err != nil {
		return 0, err
	}
	fc.file.mu.Lock()
	fc.file.hashes[index] = sha1
	fc.file.size += int64(size)
	fc.file.mu.Unlock()
	return size, nil
}

type b2FinishLargeFileRequest struct {
	ID     string   `json:"fileId"`
	Hashes []string `json:"partSha1Array"`
}

type b2FinishLargeFileResponse struct {
	Name   string `json:"fileName"`
	FileID string `json:"fileId"`
}

// FinishLargeFile wraps b2_finish_large_file.
func (l *LargeFile) FinishLargeFile(ctx context.Context) (*File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b2req := &b2FinishLargeFileRequest{
		ID:     l.id,
		Hashes: make([]string, len(l.hashes)),
	}
	b2resp := &b2FinishLargeFileResponse{}
	for k, v := range l.hashes {
		b2req.Hashes[k-1] = v
	}
	headers := map[string]string{
		"Authorization": l.b2.authToken,
	}
	if err := makeRequest(ctx, "b2_finish_large_file", "POST", l.b2.apiURI+apiV1+"b2_finish_large_file", b2req, b2resp, headers, nil); err != nil {
		return nil, err
	}
	return &File{
		Name: b2resp.Name,
		Size: l.size,
		id:   b2resp.FileID,
		b2:   l.b2,
	}, nil
}

type b2ListFileNamesRequest struct {
	BucketID     string `json:"bucketId"`
	Count        int    `json:"maxFileCount"`
	Continuation string `json:"startFileName,omitempty"`
}

type b2ListFileNamesResponse struct {
	Continuation string `json:"nextFileName"`
	Files        []struct {
		FileID string `json:"fileId"`
		Name   string `json:"fileName"`
		Size   int64  `json:"size"`
		Action string `json:"action"`
	} `json:"files"`
}

// ListFileNames wraps b2_list_file_names.
func (b *Bucket) ListFileNames(ctx context.Context, count int, continuation string) ([]*File, string, error) {
	b2req := &b2ListFileNamesRequest{
		Count:        count,
		Continuation: continuation,
		BucketID:     b.id,
	}
	b2resp := &b2ListFileNamesResponse{}
	headers := map[string]string{
		"Authorization": b.b2.authToken,
	}
	if err := makeRequest(ctx, "b2_list_file_names", "POST", b.b2.apiURI+apiV1+"b2_list_file_names", b2req, b2resp, headers, nil); err != nil {
		return nil, "", err
	}
	cont := b2resp.Continuation
	var files []*File
	for _, f := range b2resp.Files {
		files = append(files, &File{
			Name:   f.Name,
			Size:   f.Size,
			Status: f.Action,
			id:     f.FileID,
			b2:     b.b2,
		})
	}
	return files, cont, nil
}

type b2ListFileVersionsRequest struct {
	BucketID  string `json:"bucketId"`
	Count     int    `json:"maxFileCount"`
	StartName string `json:"startFileName,omitempty"`
	StartID   string `json:"startFileId,omitempty"`
}

type b2ListFileVersionsResponse struct {
	NextName string `json:"nextFileName"`
	NextID   string `json:"nextFileId"`
	Files    []struct {
		FileID string `json:"fileId"`
		Name   string `json:"fileName"`
		Size   int64  `json:"size"`
		Action string `json:"action"`
	} `json:"files"`
}

// ListFileVersions wraps b2_list_file_versions.
func (b *Bucket) ListFileVersions(ctx context.Context, count int, startName, startID string) ([]*File, string, string, error) {
	b2req := &b2ListFileVersionsRequest{
		BucketID:  b.id,
		Count:     count,
		StartName: startName,
		StartID:   startID,
	}
	b2resp := &b2ListFileVersionsResponse{}
	headers := map[string]string{
		"Authorization": b.b2.authToken,
	}
	if err := makeRequest(ctx, "b2_list_file_versions", "POST", b.b2.apiURI+apiV1+"b2_list_file_versions", b2req, b2resp, headers, nil); err != nil {
		return nil, "", "", err
	}
	var files []*File
	for _, f := range b2resp.Files {
		files = append(files, &File{
			Name:   f.Name,
			Size:   f.Size,
			Status: f.Action,
			id:     f.FileID,
			b2:     b.b2,
		})
	}
	return files, b2resp.NextName, b2resp.NextID, nil
}

// FileReader is an io.ReadCloser that downloads a file from B2.
type FileReader struct {
	io.ReadCloser
	ContentLength int
	ContentType   string
	SHA1          string
	Info          map[string]string
}

func mkRange(offset, size int64) string {
	if offset == 0 && size == 0 {
		return ""
	}
	if size == 0 {
		return fmt.Sprintf("bytes=%d-", offset)
	}
	return fmt.Sprintf("bytes=%d-%d", offset, offset+size-1)
}

// DownloadFileByName wraps b2_download_file_by_name.
func (b *Bucket) DownloadFileByName(ctx context.Context, name string, offset, size int64) (*FileReader, error) {
	url := fmt.Sprintf("%s/file/%s/%s", b.b2.downloadURI, b.Name, name)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", b.b2.authToken)
	req.Header.Set("X-Blazer-Request-ID", fmt.Sprintf("%d", atomic.AddInt64(&reqID, 1)))
	req.Header.Set("X-Blazer-Method", "b2_download_file_by_name")
	rng := mkRange(offset, size)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	cancel := make(chan struct{})
	req.Cancel = cancel
	logRequest(req, nil)
	ch := makeNetRequest(req)
	var reply httpReply
	select {
	case reply = <-ch:
	case <-ctx.Done():
		close(cancel)
		return nil, ctx.Err()
	}
	if reply.err != nil {
		return nil, reply.err
	}
	resp := reply.resp
	logResponse(resp, nil)
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return nil, mkErr(resp)
	}
	clen, err := strconv.ParseInt(reply.resp.Header.Get("Content-Length"), 10, 64)
	if err != nil {
		return nil, err
	}
	info := make(map[string]string)
	for key := range reply.resp.Header {
		if !strings.HasPrefix(key, "X-Bz-Info-") {
			continue
		}
		name := strings.TrimPrefix(key, "X-Bz-Info-")
		info[name] = reply.resp.Header.Get(key)
	}
	return &FileReader{
		ReadCloser:    reply.resp.Body,
		SHA1:          reply.resp.Header.Get("X-Bz-Content-Sha1"),
		ContentType:   reply.resp.Header.Get("Content-Type"),
		ContentLength: int(clen),
		Info:          info,
	}, nil
}
