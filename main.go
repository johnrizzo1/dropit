package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/redis/go-redis/v9"
)

var maxUploadSize int64 = 10 << 20 // 10 MB default

var s3Client *s3.Client
var s3Bucket string
var rdb *redis.Client
var turnstileSiteKey string
var turnstileSecret string

const resultsPrefix = "results/"

// Cache TTLs
const (
	runsListTTL = 60 * time.Second  // runs list refreshes every minute
	fileListTTL = 5 * time.Minute   // file listings are fairly stable
	fileBodyTTL = 30 * time.Minute  // file content rarely changes once written
)

func main() {
	s3Bucket = os.Getenv("S3_BUCKET")
	if s3Bucket == "" {
		log.Fatal("S3_BUCKET environment variable is required")
	}

	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}

	accessKey := os.Getenv("AWS_ACCESS_KEY_ID")
	secretKey := os.Getenv("AWS_SECRET_ACCESS_KEY")

	var cfg aws.Config
	var err error

	if accessKey != "" && secretKey != "" {
		cfg, err = config.LoadDefaultConfig(context.Background(),
			config.WithRegion(region),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		)
	} else {
		cfg, err = config.LoadDefaultConfig(context.Background(),
			config.WithRegion(region),
		)
	}
	if err != nil {
		log.Fatalf("Failed to load AWS config: %v", err)
	}

	// Support custom S3 endpoint (e.g. MinIO)
	s3Opts := []func(*s3.Options){}
	if endpoint := os.Getenv("S3_ENDPOINT"); endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		})
	}
	s3Client = s3.NewFromConfig(cfg, s3Opts...)

	// Redis setup
	redisAddr := os.Getenv("REDIS_URL")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb = redis.NewClient(&redis.Options{
		Addr: redisAddr,
	})
	// Retry connection a few times (valkey may still be starting)
	for i := range 5 {
		if err := rdb.Ping(context.Background()).Err(); err != nil {
			if i == 4 {
				log.Printf("WARNING: Valkey not available at %s: %v (caching disabled)", redisAddr, err)
				rdb = nil
			} else {
				time.Sleep(2 * time.Second)
			}
		} else {
			log.Printf("Valkey connected at %s", redisAddr)
			break
		}
	}

	// Turnstile CAPTCHA
	turnstileSiteKey = os.Getenv("TURNSTILE_SITE_KEY")
	turnstileSecret = os.Getenv("TURNSTILE_SECRET_KEY")
	if turnstileSiteKey != "" && turnstileSecret != "" {
		log.Printf("Turnstile CAPTCHA enabled")
	} else {
		log.Printf("WARNING: TURNSTILE_SITE_KEY / TURNSTILE_SECRET_KEY not set (CAPTCHA disabled)")
	}

	if v := os.Getenv("MAX_UPLOAD_SIZE_MB"); v != "" {
		mb, err := strconv.ParseInt(v, 10, 64)
		if err != nil || mb <= 0 {
			log.Fatalf("Invalid MAX_UPLOAD_SIZE_MB: %s", v)
		}
		maxUploadSize = mb << 20
	}
	log.Printf("Max upload size: %d MB", maxUploadSize>>20)

	http.HandleFunc("/upload", handleUpload)
	http.HandleFunc("/api/captcha-key", handleCaptchaKey)
	http.HandleFunc("/api/runs", handleListRuns)
	http.HandleFunc("/api/runs/", handleRunDetail)
	http.Handle("/", http.FileServer(http.Dir("static")))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Server starting on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// --- Cache helpers ---

func cacheGet(ctx context.Context, key string) (string, bool) {
	if rdb == nil {
		return "", false
	}
	val, err := rdb.Get(ctx, key).Result()
	if err != nil {
		return "", false
	}
	return val, true
}

func cacheSet(ctx context.Context, key string, value string, ttl time.Duration) {
	if rdb == nil {
		return
	}
	rdb.Set(ctx, key, value, ttl)
}

// --- CAPTCHA ---

func handleCaptchaKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"siteKey": turnstileSiteKey})
}

func verifyCaptcha(token string) bool {
	if turnstileSecret == "" {
		return true // CAPTCHA not configured, allow all
	}
	if token == "" {
		return false
	}

	resp, err := http.PostForm("https://challenges.cloudflare.com/turnstile/v0/siteverify",
		url.Values{
			"secret":   {turnstileSecret},
			"response": {token},
		},
	)
	if err != nil {
		log.Printf("Turnstile verification error: %v", err)
		return false
	}
	defer resp.Body.Close()

	var result struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("Turnstile decode error: %v", err)
		return false
	}
	return result.Success
}

// --- Upload ---

type uploadResponse struct {
	Success  bool   `json:"success"`
	Filename string `json:"filename,omitempty"`
	Error    string `json:"error,omitempty"`
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(uploadResponse{Error: "method not allowed"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(uploadResponse{Error: "file too large (max 10MB)"})
		return
	}

	// Verify CAPTCHA
	if !verifyCaptcha(r.FormValue("cf-turnstile-response")) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(uploadResponse{Error: "CAPTCHA verification failed"})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(uploadResponse{Error: "no file provided"})
		return
	}
	defer file.Close()

	// Generate a unique key using timestamp + original filename
	key := fmt.Sprintf("%d_%s", time.Now().UnixNano(), filepath.Base(header.Filename))

	_, err = s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:        aws.String(s3Bucket),
		Key:           aws.String(key),
		Body:          file,
		ContentLength: aws.Int64(header.Size),
		ContentType:   aws.String(header.Header.Get("Content-Type")),
	})
	if err != nil {
		log.Printf("S3 upload error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(uploadResponse{Error: "failed to upload file"})
		return
	}

	// Invalidate runs list cache on new upload
	if rdb != nil {
		rdb.Del(context.Background(), "dropit:runs")
	}

	log.Printf("Uploaded %s (%d bytes) to s3://%s/%s", header.Filename, header.Size, s3Bucket, key)
	json.NewEncoder(w).Encode(uploadResponse{Success: true, Filename: header.Filename})
}

// --- Results API ---

type runInfo struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

type fileInfo struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	Size         int64  `json:"size"`
	LastModified string `json:"last_modified"`
}

func handleListRuns(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	ctx := r.Context()

	// Check cache
	if cached, ok := cacheGet(ctx, "dropit:runs"); ok {
		w.Write([]byte(cached))
		return
	}

	var runs []runInfo
	paginator := s3.NewListObjectsV2Paginator(s3Client, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s3Bucket),
		Prefix:    aws.String(resultsPrefix),
		Delimiter: aws.String("/"),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			log.Printf("S3 list error: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "failed to list runs"})
			return
		}
		for _, cp := range page.CommonPrefixes {
			prefix := aws.ToString(cp.Prefix)
			name := strings.TrimPrefix(prefix, resultsPrefix)
			name = strings.TrimSuffix(name, "/")
			if name != "" {
				runs = append(runs, runInfo{Name: name, Prefix: prefix})
			}
		}
	}

	if runs == nil {
		runs = []runInfo{}
	}

	resp, _ := json.Marshal(map[string]any{"runs": runs})
	cacheSet(ctx, "dropit:runs", string(resp), runsListTTL)
	w.Write(resp)
}

func handleRunDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Parse: /api/runs/{name}/files or /api/runs/{name}/file?key=...
	rest := strings.TrimPrefix(r.URL.Path, "/api/runs/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "missing run name"})
		return
	}

	name := parts[0]
	if strings.Contains(name, "..") {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid run name"})
		return
	}

	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "files":
		handleRunFiles(w, r, name)
	case "file":
		handleRunFile(w, r, name)
	default:
		handleRunFiles(w, r, name)
	}
}

func handleRunFiles(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	cacheKey := "dropit:files:" + name

	// Check cache
	if cached, ok := cacheGet(ctx, cacheKey); ok {
		w.Write([]byte(cached))
		return
	}

	prefix := resultsPrefix + name + "/"
	var files []fileInfo

	paginator := s3.NewListObjectsV2Paginator(s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s3Bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			log.Printf("S3 list error: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "failed to list files"})
			return
		}
		for _, obj := range page.Contents {
			key := aws.ToString(obj.Key)
			files = append(files, fileInfo{
				Key:          key,
				Name:         path.Base(key),
				Size:         aws.ToInt64(obj.Size),
				LastModified: obj.LastModified.Format(time.RFC3339),
			})
		}
	}

	if files == nil {
		files = []fileInfo{}
	}

	resp, _ := json.Marshal(map[string]any{"files": files})
	cacheSet(ctx, cacheKey, string(resp), fileListTTL)
	w.Write(resp)
}

func handleRunFile(w http.ResponseWriter, r *http.Request, name string) {
	key := r.URL.Query().Get("key")
	if key == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "missing key parameter"})
		return
	}

	// Only allow reading from results/ prefix
	if !strings.HasPrefix(key, resultsPrefix) || strings.Contains(key, "..") {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "access denied"})
		return
	}

	ctx := r.Context()
	cacheKey := "dropit:body:" + key

	// Set content type based on extension
	ext := strings.ToLower(path.Ext(key))
	switch ext {
	case ".jsonl":
		w.Header().Set("Content-Type", "application/x-ndjson")
	case ".md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	case ".txt":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	case ".json":
		w.Header().Set("Content-Type", "application/json")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}

	// Check cache
	if cached, ok := cacheGet(ctx, cacheKey); ok {
		w.Write([]byte(cached))
		return
	}

	output, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s3Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("S3 get error for %s: %v", key, err)
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "file not found"})
		return
	}
	defer output.Body.Close()

	body, err := io.ReadAll(output.Body)
	if err != nil {
		log.Printf("S3 read error for %s: %v", key, err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to read file"})
		return
	}

	cacheSet(ctx, cacheKey, string(body), fileBodyTTL)
	w.Write(body)
}
