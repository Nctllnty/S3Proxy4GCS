package gov2

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestObjectCRUD(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	key := GenerateTestKey(testEnv, "gov2-crud")
	content := "Hello from Go V2 SDK test!"

	t.Cleanup(func() { Cleanup(t, client, bucket, key) })

	// PutObject
	_, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   strings.NewReader(content),
	})
	if err != nil {
		t.Fatalf("PutObject failed: %v", err)
	}

	// GetObject
	getOut, err := client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject failed: %v", err)
	}
	body, _ := io.ReadAll(getOut.Body)
	getOut.Body.Close()
	if string(body) != content {
		t.Fatalf("body mismatch: got %q, want %q", string(body), content)
	}

	// HeadObject
	headOut, err := client.HeadObject(context.TODO(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject failed: %v", err)
	}
	if headOut.ContentLength == nil || *headOut.ContentLength == 0 {
		t.Errorf("HeadObject returned zero ContentLength")
	}

	// DeleteObject
	_, err = client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("DeleteObject failed: %v", err)
	}

	// GetObject after delete — expect error
	_, err = client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		t.Fatal("GetObject after delete should have failed")
	}
}

func TestMultipartUpload(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	key := GenerateTestKey(testEnv, "gov2-multipart")

	t.Cleanup(func() { Cleanup(t, client, bucket, key) })

	part1 := strings.Repeat("A", 5*1024*1024)
	part2 := "Final part"

	createOut, err := client.CreateMultipartUpload(context.TODO(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload failed: %v", err)
	}

	up1, err := client.UploadPart(context.TODO(), &s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: createOut.UploadId, PartNumber: aws.Int32(1),
		Body: strings.NewReader(part1),
	})
	if err != nil {
		t.Fatalf("UploadPart #1 failed: %v", err)
	}

	up2, err := client.UploadPart(context.TODO(), &s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: createOut.UploadId, PartNumber: aws.Int32(2),
		Body: strings.NewReader(part2),
	})
	if err != nil {
		t.Fatalf("UploadPart #2 failed: %v", err)
	}

	_, err = client.CompleteMultipartUpload(context.TODO(), &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: createOut.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: []types.CompletedPart{
				{PartNumber: aws.Int32(1), ETag: up1.ETag},
				{PartNumber: aws.Int32(2), ETag: up2.ETag},
			},
		},
	})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload failed: %v", err)
	}

	getOut, err := client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject after multipart failed: %v", err)
	}
	merged, _ := io.ReadAll(getOut.Body)
	getOut.Body.Close()
	if !bytes.Equal(merged, []byte(part1+part2)) {
		t.Fatalf("Multipart body mismatch: got %d bytes, want %d", len(merged), len(part1)+len(part2))
	}
}

func TestListObjectsV2(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	prefix := testEnv.TestPrefix + "gov2-list/"

	keys := []string{prefix + "a", prefix + "b", prefix + "c"}
	t.Cleanup(func() { Cleanup(t, client, bucket, keys...) })

	for _, key := range keys {
		_, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
			Body: strings.NewReader("list test"),
		})
		if err != nil {
			t.Fatalf("PutObject(%s) failed: %v", key, err)
		}
	}

	listOut, err := client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String(prefix),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2 failed: %v", err)
	}
	if len(listOut.Contents) < 3 {
		t.Fatalf("Expected at least 3 objects, got %d", len(listOut.Contents))
	}
}

func TestDeleteObjects(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	key1 := GenerateTestKey(testEnv, "gov2-delobj-1")
	key2 := GenerateTestKey(testEnv, "gov2-delobj-2")
	key3 := GenerateTestKey(testEnv, "gov2-delobj-3")

	t.Cleanup(func() { Cleanup(t, client, bucket, key1, key2, key3) })

	// Create 3 objects
	for _, key := range []string{key1, key2, key3} {
		_, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
			Body: strings.NewReader("delete-objects test"),
		})
		if err != nil {
			t.Fatalf("PutObject(%s) failed: %v", key, err)
		}
	}

	// DeleteObjects — bulk delete key1 and key2
	delOut, err := client.DeleteObjects(context.TODO(), &s3.DeleteObjectsInput{
		Bucket: aws.String(bucket),
		Delete: &types.Delete{
			Objects: []types.ObjectIdentifier{
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
		_, err := client.HeadObject(context.TODO(), &s3.HeadObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
		})
		if err == nil {
			t.Fatalf("HeadObject(%s) should have failed after DeleteObjects", key)
		}
	}

	// Verify key3 still exists
	_, err = client.HeadObject(context.TODO(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key3),
	})
	if err != nil {
		t.Fatalf("HeadObject(%s) should still exist: %v", key3, err)
	}
}

func TestStorageClass(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	key := GenerateTestKey(testEnv, "gov2-storageclass")

	t.Cleanup(func() { Cleanup(t, client, bucket, key) })

	_, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		Body:         strings.NewReader("storage class test"),
		StorageClass: types.StorageClassStandardIa,
	})
	if err != nil {
		t.Fatalf("PutObject with STANDARD_IA failed: %v", err)
	}

	_, err = client.HeadObject(context.TODO(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject failed: %v", err)
	}
}

func TestCopyObject(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	srcKey := GenerateTestKey(testEnv, "gov2-copy-src")
	dstKey := GenerateTestKey(testEnv, "gov2-copy-dst")
	content := "CopyObject test content"

	t.Cleanup(func() { Cleanup(t, client, bucket, srcKey, dstKey) })

	// Put source object
	_, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(srcKey),
		Body: strings.NewReader(content),
	})
	if err != nil {
		t.Fatalf("PutObject (source) failed: %v", err)
	}

	// CopyObject
	_, err = client.CopyObject(context.TODO(), &s3.CopyObjectInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(bucket + "/" + srcKey),
	})
	if err != nil {
		t.Fatalf("CopyObject failed: %v", err)
	}

	// Verify destination content
	getOut, err := client.GetObject(context.TODO(), &s3.GetObjectInput{
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
	key := GenerateTestKey(testEnv, "gov2-abort-multipart")

	t.Cleanup(func() { Cleanup(t, client, bucket, key) })

	// Create multipart upload
	createOut, err := client.CreateMultipartUpload(context.TODO(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload failed: %v", err)
	}

	// Upload a part
	_, err = client.UploadPart(context.TODO(), &s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: createOut.UploadId, PartNumber: aws.Int32(1),
		Body: strings.NewReader(strings.Repeat("B", 5*1024*1024)),
	})
	if err != nil {
		t.Fatalf("UploadPart failed: %v", err)
	}

	// Abort the multipart upload
	_, err = client.AbortMultipartUpload(context.TODO(), &s3.AbortMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		UploadId: createOut.UploadId,
	})
	if err != nil {
		t.Fatalf("AbortMultipartUpload failed: %v", err)
	}

	// Verify: ListParts should fail since upload is aborted
	_, err = client.ListParts(context.TODO(), &s3.ListPartsInput{
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
	key := GenerateTestKey(testEnv, "gov2-restore")

	t.Cleanup(func() { Cleanup(t, client, bucket, key) })

	// Put object with GLACIER storage class
	_, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		Body:         strings.NewReader("restore test content"),
		StorageClass: types.StorageClassGlacier,
	})
	if err != nil {
		t.Fatalf("PutObject with GLACIER failed: %v", err)
	}

	// RestoreObject — proxy returns synthetic 200/202
	_, err = client.RestoreObject(context.TODO(), &s3.RestoreObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		RestoreRequest: &types.RestoreRequest{
			Days: aws.Int32(7),
		},
	})
	if err != nil {
		t.Fatalf("RestoreObject failed: %v", err)
	}
}

func TestListObjectVersions(t *testing.T) {
	client := NewS3Client(t, testEnv)
	bucket := testEnv.TestBucket
	key := GenerateTestKey(testEnv, "gov2-list-versions")

	t.Cleanup(func() { Cleanup(t, client, bucket, key) })

	// Put first version
	_, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		Body: strings.NewReader("version 1 content"),
	})
	if err != nil {
		t.Fatalf("PutObject (v1) failed: %v", err)
	}

	// Put second version (overwrite with different content)
	_, err = client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
		Body: strings.NewReader("version 2 content"),
	})
	if err != nil {
		t.Fatalf("PutObject (v2) failed: %v", err)
	}

	// ListObjectVersions
	listOut, err := client.ListObjectVersions(context.TODO(), &s3.ListObjectVersionsInput{
		Bucket: aws.String(bucket), Prefix: aws.String(key),
	})
	if err != nil {
		t.Fatalf("ListObjectVersions failed: %v", err)
	}

	// Filter versions matching our exact key
	var matched []types.ObjectVersion
	for _, v := range listOut.Versions {
		if aws.ToString(v.Key) == key {
			matched = append(matched, v)
		}
	}

	// If fewer than 2 versions, bucket likely doesn't have versioning enabled — skip
	if len(matched) < 2 {
		t.Skip("ListObjectVersions returned fewer than 2 versions; bucket may not have versioning enabled")
	}

	// Verify each version has a non-empty VersionId and correct Key
	for i, v := range matched {
		if aws.ToString(v.VersionId) == "" {
			t.Errorf("Version[%d] has empty VersionId", i)
		}
		if aws.ToString(v.Key) != key {
			t.Errorf("Version[%d] Key = %q, want %q", i, aws.ToString(v.Key), key)
		}
	}
}
