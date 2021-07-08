// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package minioclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/ioutil"

	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/zeebo/errs"
)

// MinioError is class for minio errors.
var MinioError = errs.Class("minio")

// Config is the setup for a particular client.
type Config struct {
	S3Gateway     string
	Satellite     string
	AccessKey     string
	SecretKey     string
	APIKey        string
	EncryptionKey string
	NoSSL         bool
	ConfigDir     string
}

// Client is the common interface for different implementations.
type Client interface {
	MakeBucket(ctx context.Context, bucket, region string) error
	RemoveBucket(ctx context.Context, bucket string) error
	ListBuckets(ctx context.Context) ([]string, error)

	Upload(ctx context.Context, bucket, objectName string, data []byte) error
	Download(ctx context.Context, bucket, objectName string, buffer []byte) ([]byte, error)
	Delete(ctx context.Context, bucket, objectName string) error
	ListObjects(ctx context.Context, bucket, prefix string) []string
}

// Minio implements basic S3 Client with minio.
type Minio struct {
	API *minio.Client
}

// NewMinio creates new Client.
func NewMinio(conf Config) (Client, error) {
	client, err := minio.New(conf.S3Gateway, &minio.Options{
		Creds:  credentials.NewStaticV4(conf.AccessKey, conf.SecretKey, ""),
		Secure: !conf.NoSSL,
	})
	if err != nil {
		return nil, MinioError.Wrap(err)
	}
	return &Minio{client}, nil
}

// MakeBucket makes a new bucket.
func (client *Minio) MakeBucket(ctx context.Context, bucket, region string) error {
	err := client.API.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: region})
	if err != nil {
		return MinioError.Wrap(err)
	}
	return nil
}

// RemoveBucket removes a bucket.
func (client *Minio) RemoveBucket(ctx context.Context, bucket string) error {
	err := client.API.RemoveBucket(ctx, bucket)
	if err != nil {
		return MinioError.Wrap(err)
	}
	return nil
}

// ListBuckets lists all buckets.
func (client *Minio) ListBuckets(ctx context.Context) ([]string, error) {
	buckets, err := client.API.ListBuckets(ctx)
	if err != nil {
		return nil, MinioError.Wrap(err)
	}

	names := []string{}
	for _, bucket := range buckets {
		names = append(names, bucket.Name)
	}
	return names, nil
}

// Upload uploads object data to the specified path.
func (client *Minio) Upload(ctx context.Context, bucket, objectName string, data []byte) error {
	_, err := client.API.PutObject(
		ctx, bucket, objectName,
		bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return MinioError.Wrap(err)
	}
	return nil
}

// UploadMultipart uses multipart uploads, has hardcoded threshold.
func (client *Minio) UploadMultipart(ctx context.Context, bucket, objectName string, data []byte, partSize int, threshold int) error {
	_, err := client.API.PutObject(
		ctx, bucket, objectName,
		bytes.NewReader(data), -1,
		minio.PutObjectOptions{
			ContentType: "application/octet-stream",
			PartSize:    uint64(partSize),
		})
	if err != nil {
		return MinioError.Wrap(err)
	}
	return nil
}

// Download downloads object data.
func (client *Minio) Download(ctx context.Context, bucket, objectName string, buffer []byte) ([]byte, error) {
	reader, err := client.API.GetObject(ctx, bucket, objectName, minio.GetObjectOptions{})
	if err != nil {
		return nil, MinioError.Wrap(err)
	}
	defer func() { _ = reader.Close() }()

	n, err := reader.Read(buffer[:cap(buffer)])
	if !errors.Is(err, io.EOF) {
		rest, err := ioutil.ReadAll(reader)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		if err != nil {
			return nil, MinioError.Wrap(err)
		}
		buffer = append(buffer, rest...)
		n = len(buffer)
	}

	buffer = buffer[:n]
	return buffer, nil
}

// Delete deletes object.
func (client *Minio) Delete(ctx context.Context, bucket, objectName string) error {
	err := client.API.RemoveObject(ctx, bucket, objectName, minio.RemoveObjectOptions{})
	if err != nil {
		return MinioError.Wrap(err)
	}
	return nil
}

// ListObjects lists objects.
func (client *Minio) ListObjects(ctx context.Context, bucket, prefix string) []string {
	var names []string
	for message := range client.API.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix}) {
		names = append(names, message.Key)
	}
	return names
}
