# S3 to GCS Proxy - Test Cases

This document defines all tracked features of the S3 to GCS Proxy, including supported standard operations, custom interceptor features, and deferred/unsupported items.

---

## 🟢 Standard Data-Plane Features (Reverse Proxy)

These features pass through the proxy transparently to GCS. The proxy preserves streaming behavior and applies connection pooling.

### 1. Simple Object Operations
-   **`PutObject`**
-   **`GetObject`**
-   **`HeadObject`**
-   **`DeleteObject`**
-   **`DeleteObjects`** (Multi-Object Delete, up to 1000 objects per request)
-   **`ListObjectsV2`**
-   **`DeleteObjects`** (Multi-Object Delete)

### 2. Multipart Uploads
-   **`CreateMultipartUpload`**
-   **`UploadPart`**
-   **`CompleteMultipartUpload`**
-   **`AbortMultipartUpload`**

---

## 🟡 Custom Intercepted Features (Proxy Translations)

These features are intercepted by the proxy to translate between AWS XML and GCS JSON or headers.

### 3. Lifecycle Management
-   **`PutBucketLifecycleConfiguration`**
    -   Standard rules (Expiration, Transition to Storage Class).
    -   Multiple transitions.

### 4. CORS Configuration
-   **`PutBucketCors`**
-   **`GetBucketCors`**
-   **`DeleteBucketCors`**

### 5. Bucket Logging
-   **`PutBucketLogging`**
-   **`GetBucketLogging`**
-   **`DeleteBucketLogging`**

### 6. Static Website Configuration
-   **`PutBucketWebsite`**

### 7. Object Tagging
-   **`PutObjectTagging`** (Maps to GCS Custom Metadata with OCC).
-   **`GetObjectTagging`**
-   **`DeleteObjectTagging`**

### 8. Storage Class Translation
-   **`x-amz-storage-class`** header re-writing (e.g. `STANDARD_IA` ➔ `NEARLINE`).

### 9. Versioning Interop
-   **`ListObjectVersions`** and **`HeadObject` version mapping** (Generation to VersionId).

---

## 🔴 Unsupported / Deferred Features

These features are known failures or formally deferred due to performance or architectural limitations.

### 10. Multi-Object Delete
-   **`DeleteObjects`** — GCS XML API natively supports `POST /?delete` bulk delete (up to 1000 objects). Passes through the proxy transparently with SigV4 re-signing. **[Implemented ✅]**

### 11. Flexible Checksums (aws-chunked)
-   **`aws-chunked` trailers for checksums** (Deferred to client workarounds: `AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED`).

### 12. Upload Part Copy
-   **`UploadPartCopy`** cross-object multipart copies (GCS S3 API limitation).

### 13. Restore Object
-   **`RestoreObject`** — synthetic response implemented in proxy (v1.5+). GCS considers archive objects "live" so a real thaw is unnecessary; the proxy returns `200 OK` for live keys, `404 NoSuchKey` for missing keys, and `501 NotImplemented` for non-POST verbs on `?restore`. Regression covered by `integration_tests/restore_object_test.go` and `main_test.go`.
