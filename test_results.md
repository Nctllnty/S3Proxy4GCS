# S3 to GCS Proxy - Test Results

This document summarizes the current verification status for the S3 to GCS Proxy based on execution logs with real HMAC keys.

---

## 📊 Summary of Test Runs (Authenticated Real GCS Mode)

| Category | Test Name | Result | Log File Source | Notes |
| :--- | :--- | :--- | :--- | :--- |
| **CORS** | `TestPutCorsWithAWSSDK` | ✅ `PASS` | [Log](file:///Users/deckardy/gitlab/s3proxy4gcs/log/TestPutCorsWithAWSSDK.log) | Put, Get, and Delete verified |
| **Data-Plane** | `TestStandardDataPlaneWithAWSSDK` | ✅ `PASS` | [Log](file:///Users/deckardy/gitlab/s3proxy4gcs/log/TestStandardDataPlaneWithAWSSDK.log) | Re-signed & stripped `x-id` |
| **Multipart** | `TestMultipartUploadWithAWSSDK` | ✅ `PASS` | [Log](file:///Users/deckardy/gitlab/s3proxy4gcs/log/TestMultipartUploadWithAWSSDK.log) | Created, Uploaded, Aborted |
| **Lifecycle** | `TestPutLifecycleWithAWSSDK` | ✅ `PASS` | [Log](file:///Users/deckardy/gitlab/s3proxy4gcs/log/TestPutLifecycleWithAWSSDK.log) | Translated successfully |
| **Lifecycle** | `TestPutLifecycleMultipleTransitionsAWSSDK` | ✅ `PASS` | [Log](file:///Users/deckardy/gitlab/s3proxy4gcs/log/TestPutLifecycleMultipleTransitionsAWSSDK.log) | Translated successfully |
| **Logging** | `TestPutLoggingWithAWSSDK` | ✅ `PASS` | [Log](file:///Users/deckardy/gitlab/s3proxy4gcs/log/TestPutLoggingWithAWSSDK.log) | Put and Get verified |
| **Storage Class**| `TestPutObjectStorageClassWithAWSSDK` | ✅ `PASS` | [Log](file:///Users/deckardy/gitlab/s3proxy4gcs/log/TestPutObjectStorageClassWithAWSSDK.log) | Translated `STANDARD_IA` to `NEARLINE` |
| **Tagging** | `TestPutObjectTaggingWithAWSSDK` | ✅ `PASS` | [Log](file:///Users/deckardy/gitlab/s3proxy4gcs/log/TestPutObjectTaggingWithAWSSDK.log) | Put and Get verified (OCC metadata) |
| **Versioning** | `TestListObjectVersionsWithAWSSDK` | ✅ `PASS` | [Log](file:///Users/deckardy/gitlab/s3proxy4gcs/log/TestListObjectVersionsWithAWSSDK.log) | Interop header injected |
| **Versioning** | `TestHeadObjectVersionIdWithAWSSDK` | ✅ `PASS` | [Log](file:///Users/deckardy/gitlab/s3proxy4gcs/log/TestHeadObjectVersionIdWithAWSSDK.log) | mapped `x-goog-generation` to `x-amz-version-id` |
| **Website** | `TestPutWebsiteWithAWSSDK` | ✅ `PASS` | [Log](file:///Users/deckardy/gitlab/s3proxy4gcs/log/TestPutWebsiteWithAWSSDK.log) | Translated successfully |

---


## 🚫 Unsupported Features (Manual Assessment)

These features were assessed via `aws4gcs/test_report.md` and `solutions.md` findings without active automated tests in the workspace module:

| Category | Feature | Status | Workaround / Explanation |
| :--- | :--- | :--- | :--- |
| **Multipart** | `UploadPartCopy` | ❌ Fail | GCS limitation for cross-object multipart copies. |
| **Restore** | `RestoreObject` | ✅ Synthesised (v1.5+) | Proxy returns 200 OK for live keys / 404 NoSuchKey for missing keys / 501 for non-POST verbs. GCS archive objects are always directly readable; see `integration_tests/restore_object_test.go`. |
| **Lifecycle** | `Reject unsupported filters` | ✅ Handled | Blocked with S3 XML error. |
| **Integrity** | `aws-chunked` trailers | 🚫 Deferred | Use `AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED`. |
