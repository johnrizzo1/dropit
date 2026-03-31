package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var maxUploadSize int64 = 10 << 20 // 10 MB default

var s3Client *s3.Client
var s3Bucket string

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

	if v := os.Getenv("MAX_UPLOAD_SIZE_MB"); v != "" {
		mb, err := strconv.ParseInt(v, 10, 64)
		if err != nil || mb <= 0 {
			log.Fatalf("Invalid MAX_UPLOAD_SIZE_MB: %s", v)
		}
		maxUploadSize = mb << 20
	}
	log.Printf("Max upload size: %d MB", maxUploadSize>>20)

	http.Handle("/", http.FileServer(http.Dir("static")))
	http.HandleFunc("/upload", handleUpload)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Server starting on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

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

	log.Printf("Uploaded %s (%d bytes) to s3://%s/%s", header.Filename, header.Size, s3Bucket, key)
	json.NewEncoder(w).Encode(uploadResponse{Success: true, Filename: header.Filename})
}
