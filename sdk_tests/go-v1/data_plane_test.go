package gov1

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
)

func TestObjectCRUD(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	key := GenerateTestKey(testEnv, "gov1-crud")
	content := "Hello from Go V1 SDK!"

	t.Cleanup(func() { Cleanup(t, client, bucket, key) })

	_, err := client.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		Body: strings.NewReader(content),
	})
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	getOut, err := client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	body, _ := io.ReadAll(getOut.Body)
	getOut.Body.Close()
	if string(body) != content {
		t.Fatalf("body mismatch: got %q, want %q", string(body), content)
	}

	_, err = client.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject failed: %v", err)
	}

	_, err = client.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}

	_, err = client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err == nil {
		t.Fatal("GetObject after delete should have failed")
	}
}

func TestMultipartUpload(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	key := GenerateTestKey(testEnv, "gov1-multipart")

	t.Cleanup(func() { Cleanup(t, client, bucket, key) })

	part1 := strings.Repeat("A", 5*1024*1024)
	part2 := "Final part"

	createOut, err := client.CreateMultipartUpload(&s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload failed: %v", err)
	}

	up1, err := client.UploadPart(&s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: createOut.UploadId, PartNumber: aws.Int64(1),
		Body: strings.NewReader(part1),
	})
	if err != nil {
		t.Fatalf("UploadPart #1 failed: %v", err)
	}

	up2, err := client.UploadPart(&s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: createOut.UploadId, PartNumber: aws.Int64(2),
		Body: strings.NewReader(part2),
	})
	if err != nil {
		t.Fatalf("UploadPart #2 failed: %v", err)
	}

	_, err = client.CompleteMultipartUpload(&s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: createOut.UploadId,
		MultipartUpload: &s3.CompletedMultipartUpload{
			Parts: []*s3.CompletedPart{
				{PartNumber: aws.Int64(1), ETag: up1.ETag},
				{PartNumber: aws.Int64(2), ETag: up2.ETag},
			},
		},
	})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload failed: %v", err)
	}

	getOut, err := client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject after multipart failed: %v", err)
	}
	merged, _ := io.ReadAll(getOut.Body)
	getOut.Body.Close()
	if !bytes.Equal(merged, []byte(part1+part2)) {
		t.Fatalf("body mismatch: got %d bytes, want %d", len(merged), len(part1)+len(part2))
	}
}

func TestDeleteObjects(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	key1 := GenerateTestKey(testEnv, "gov1-delobj-1")
	key2 := GenerateTestKey(testEnv, "gov1-delobj-2")
	key3 := GenerateTestKey(testEnv, "gov1-delobj-3")

	t.Cleanup(func() { Cleanup(t, client, bucket, key1, key2, key3) })

	// Create 3 objects
	for _, key := range []string{key1, key2, key3} {
		_, err := client.PutObject(&s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
			Body: strings.NewReader("delete-objects test"),
		})
		if err != nil {
			t.Fatalf("PutObject(%s) failed: %v", key, err)
		}
	}

	// DeleteObjects — bulk delete key1 and key2
	delOut, err := client.DeleteObjects(&s3.DeleteObjectsInput{
		Bucket: aws.String(bucket),
		Delete: &s3.Delete{
			Objects: []*s3.ObjectIdentifier{
				{Key: aws.String(key1)},
				{Key: aws.String(key2)},
			},
			Quiet: aws.Bool(false),
		},
	})
	if err != nil {
		t.Fatalf("DeleteObjects failed: %v", err)
	}
	if len(delOut.Errors) > 0 {
		t.Fatalf("DeleteObjects returned errors: %v", delOut.Errors)
	}
	if len(delOut.Deleted) != 2 {
		t.Fatalf("Expected 2 deleted, got %d", len(delOut.Deleted))
	}

	// Verify key1 and key2 are gone
	for _, key := range []string{key1, key2} {
		_, err := client.HeadObject(&s3.HeadObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
		})
		if err == nil {
			t.Fatalf("HeadObject(%s) should have failed after DeleteObjects", key)
		}
	}

	// Verify key3 still exists
	_, err = client.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key3),
	})
	if err != nil {
		t.Fatalf("HeadObject(%s) should still exist: %v", key3, err)
	}
}

func TestStorageClass(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	key := GenerateTestKey(testEnv, "gov1-storageclass")

	t.Cleanup(func() { Cleanup(t, client, bucket, key) })

	_, err := client.PutObject(&s3.PutObjectInput{
		Bucket:       aws.String(bucket),
		Key:          aws.String(key),
		Body:         strings.NewReader("storage class test"),
		StorageClass: aws.String("STANDARD_IA"),
	})
	if err != nil {
		t.Fatalf("PutObject with STANDARD_IA failed: %v", err)
	}

	_, err = client.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject failed: %v", err)
	}
}

func TestListObjectsV2(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	prefix := testEnv.TestPrefix + "gov1-list/"

	keys := []string{prefix + "a", prefix + "b", prefix + "c"}
	t.Cleanup(func() { Cleanup(t, client, bucket, keys...) })

	for _, key := range keys {
		_, err := client.PutObject(&s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
			Body: strings.NewReader("list test"),
		})
		if err != nil {
			t.Fatalf("PutObject(%s) failed: %v", key, err)
		}
	}

	listOut, err := client.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String(prefix),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2 failed: %v", err)
	}
	if len(listOut.Contents) < 3 {
		t.Fatalf("Expected >=3 objects, got %d", len(listOut.Contents))
	}
}

func TestCopyObject(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	srcKey := GenerateTestKey(testEnv, "gov1-copy-src")
	dstKey := GenerateTestKey(testEnv, "gov1-copy-dst")
	content := "CopyObject test content"

	t.Cleanup(func() { Cleanup(t, client, bucket, srcKey, dstKey) })

	_, err := client.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(srcKey),
		Body: strings.NewReader(content),
	})
	if err != nil {
		t.Fatalf("PutObject (source) failed: %v", err)
	}

	_, err = client.CopyObject(&s3.CopyObjectInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(bucket + "/" + srcKey),
	})
	if err != nil {
		t.Fatalf("CopyObject failed: %v", err)
	}

	getOut, err := client.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(dstKey),
	})
	if err != nil {
		t.Fatalf("GetObject (copy dest) failed: %v", err)
	}
	body, _ := io.ReadAll(getOut.Body)
	getOut.Body.Close()
	if string(body) != content {
		t.Fatalf("CopyObject body mismatch: got %q, want %q", string(body), content)
	}
}

func TestAbortMultipartUpload(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	key := GenerateTestKey(testEnv, "gov1-abort-multipart")

	t.Cleanup(func() { Cleanup(t, client, bucket, key) })

	createOut, err := client.CreateMultipartUpload(&s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload failed: %v", err)
	}

	_, err = client.UploadPart(&s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: createOut.UploadId, PartNumber: aws.Int64(1),
		Body: strings.NewReader(strings.Repeat("B", 5*1024*1024)),
	})
	if err != nil {
		t.Fatalf("UploadPart failed: %v", err)
	}

	_, err = client.AbortMultipartUpload(&s3.AbortMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: createOut.UploadId,
	})
	if err != nil {
		t.Fatalf("AbortMultipartUpload failed: %v", err)
	}

	_, err = client.ListParts(&s3.ListPartsInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: createOut.UploadId,
	})
	if err == nil {
		t.Fatal("ListParts after AbortMultipartUpload should have failed")
	}
}

func TestRestoreObject(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	key := GenerateTestKey(testEnv, "gov1-restore")

	t.Cleanup(func() { Cleanup(t, client, bucket, key) })

	_, err := client.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		Body:         strings.NewReader("restore test content"),
		StorageClass: aws.String("GLACIER"),
	})
	if err != nil {
		t.Fatalf("PutObject with GLACIER failed: %v", err)
	}

	_, err = client.RestoreObject(&s3.RestoreObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		RestoreRequest: &s3.RestoreRequest{
			Days: aws.Int64(7),
		},
	})
	if err != nil {
		t.Fatalf("RestoreObject failed: %v", err)
	}
}
