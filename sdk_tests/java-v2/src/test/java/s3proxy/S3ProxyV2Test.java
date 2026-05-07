package s3proxy;

import org.junit.jupiter.api.*;
import software.amazon.awssdk.auth.credentials.AwsBasicCredentials;
import software.amazon.awssdk.auth.credentials.StaticCredentialsProvider;
import software.amazon.awssdk.core.sync.RequestBody;
import software.amazon.awssdk.regions.Region;
import software.amazon.awssdk.services.s3.S3Client;
import software.amazon.awssdk.services.s3.model.*;

import software.amazon.awssdk.services.s3.S3Configuration;

import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.util.List;

import static org.junit.jupiter.api.Assertions.*;

/**
 * Functional tests using AWS Java SDK V2 (2.20.0).
 */
@TestMethodOrder(MethodOrderer.OrderAnnotation.class)
public class S3ProxyV2Test {

    private static S3Client s3;
    // SDK_TEST_BUCKET is the default GCS bucket for Java V2 SDK compatibility tests.
    // Override via TEST_BUCKET env var for different environments (e.g. production).
    private static final String SDK_TEST_BUCKET = System.getenv("TEST_BUCKET") != null
            ? System.getenv("TEST_BUCKET") : "s3proxy-sdk-java-v2";
    private static String bucket;
    private static String prefix;

    @BeforeAll
    static void setup() {
        String endpoint = System.getenv("PROXY_ENDPOINT");
        String access = System.getenv("GCS_HMAC_ACCESS");
        String secret = System.getenv("GCS_HMAC_SECRET");
        bucket = SDK_TEST_BUCKET;
        prefix = System.getenv("TEST_PREFIX") != null ? System.getenv("TEST_PREFIX") : "";

        assertNotNull(endpoint, "PROXY_ENDPOINT required");
        assertNotNull(access, "GCS_HMAC_ACCESS required");

        s3 = S3Client.builder()
                .endpointOverride(URI.create(endpoint))
                .region(Region.US_EAST_1)
                .credentialsProvider(StaticCredentialsProvider.create(
                        AwsBasicCredentials.create(access, secret)))
                .forcePathStyle(true)
                .serviceConfiguration(S3Configuration.builder()
                        .chunkedEncodingEnabled(false)
                        .build())
                .build();
    }

    private String testKey(String suffix) {
        return prefix + "javav2-" + suffix + "-" + System.nanoTime();
    }

    // ---- Data Plane ----

    @Test
    @Order(1)
    void testObjectCRUD() {
        String key = testKey("crud");
        String content = "Hello from Java V2 SDK!";

        s3.putObject(PutObjectRequest.builder().bucket(bucket).key(key).build(),
                RequestBody.fromString(content));

        var getResp = s3.getObjectAsBytes(GetObjectRequest.builder().bucket(bucket).key(key).build());
        assertEquals(content, getResp.asUtf8String());

        var head = s3.headObject(HeadObjectRequest.builder().bucket(bucket).key(key).build());
        assertTrue(head.contentLength() > 0);

        s3.deleteObject(DeleteObjectRequest.builder().bucket(bucket).key(key).build());

        assertThrows(NoSuchKeyException.class, () ->
                s3.getObject(GetObjectRequest.builder().bucket(bucket).key(key).build()));
    }

    @Test
    @Order(2)
    void testMultipartUpload() {
        String key = testKey("multipart");
        byte[] part1 = new byte[5 * 1024 * 1024];
        java.util.Arrays.fill(part1, (byte) 'A');
        byte[] part2 = "Final".getBytes(StandardCharsets.UTF_8);

        var create = s3.createMultipartUpload(CreateMultipartUploadRequest.builder()
                .bucket(bucket).key(key).build());
        String uploadId = create.uploadId();

        try {
            var r1 = s3.uploadPart(UploadPartRequest.builder()
                    .bucket(bucket).key(key).uploadId(uploadId).partNumber(1)
                    .contentLength((long) part1.length).build(), RequestBody.fromBytes(part1));

            var r2 = s3.uploadPart(UploadPartRequest.builder()
                    .bucket(bucket).key(key).uploadId(uploadId).partNumber(2)
                    .contentLength((long) part2.length).build(), RequestBody.fromBytes(part2));

            s3.completeMultipartUpload(CompleteMultipartUploadRequest.builder()
                    .bucket(bucket).key(key).uploadId(uploadId)
                    .multipartUpload(CompletedMultipartUpload.builder().parts(
                            CompletedPart.builder().partNumber(1).eTag(r1.eTag()).build(),
                            CompletedPart.builder().partNumber(2).eTag(r2.eTag()).build()
                    ).build()).build());

            var obj = s3.getObjectAsBytes(GetObjectRequest.builder().bucket(bucket).key(key).build());
            assertEquals(part1.length + part2.length, obj.asByteArray().length);
        } finally {
            s3.deleteObject(DeleteObjectRequest.builder().bucket(bucket).key(key).build());
        }
    }

    @Test
    @Order(3)
    void testListObjectsV2() {
        String listPrefix = prefix + "javav2-list/";
        String[] keys = {listPrefix + "a", listPrefix + "b", listPrefix + "c"};

        try {
            for (String key : keys) {
                s3.putObject(PutObjectRequest.builder().bucket(bucket).key(key).build(),
                        RequestBody.fromString("list test"));
            }
            var result = s3.listObjectsV2(ListObjectsV2Request.builder()
                    .bucket(bucket).prefix(listPrefix).build());
            assertTrue(result.contents().size() >= 3);
        } finally {
            for (String key : keys) {
                s3.deleteObject(DeleteObjectRequest.builder().bucket(bucket).key(key).build());
            }
        }
    }

    @Test
    @Order(4)
    void testStorageClass() {
        String key = testKey("storageclass");
        try {
            s3.putObject(PutObjectRequest.builder()
                            .bucket(bucket).key(key)
                            .storageClass(StorageClass.STANDARD_IA)
                            .build(),
                    RequestBody.fromString("storage class test"));

            var head = s3.headObject(HeadObjectRequest.builder().bucket(bucket).key(key).build());
            assertTrue(head.contentLength() > 0);
        } finally {
            s3.deleteObject(DeleteObjectRequest.builder().bucket(bucket).key(key).build());
        }
    }

    @Test
    @Order(5)
    void testDeleteObjects() {
        String key1 = testKey("delobj-1");
        String key2 = testKey("delobj-2");
        String key3 = testKey("delobj-3");

        try {
            // Create 3 objects
            for (String key : new String[]{key1, key2, key3}) {
                s3.putObject(PutObjectRequest.builder().bucket(bucket).key(key).build(),
                        RequestBody.fromString("delete-objects test"));
            }

            // DeleteObjects — bulk delete key1 and key2
            var delResp = s3.deleteObjects(DeleteObjectsRequest.builder()
                    .bucket(bucket)
                    .delete(Delete.builder()
                            .objects(
                                    ObjectIdentifier.builder().key(key1).build(),
                                    ObjectIdentifier.builder().key(key2).build())
                            .quiet(false)
                            .build())
                    .build());
            assertEquals(2, delResp.deleted().size());
            assertTrue(delResp.errors().isEmpty());

            // Verify key1 and key2 are gone
            for (String key : new String[]{key1, key2}) {
                final String k = key;
                assertThrows(NoSuchKeyException.class, () ->
                        s3.headObject(HeadObjectRequest.builder().bucket(bucket).key(k).build()));
            }

            // Verify key3 still exists
            var head = s3.headObject(HeadObjectRequest.builder().bucket(bucket).key(key3).build());
            assertTrue(head.contentLength() > 0);
        } finally {
            for (String key : new String[]{key1, key2, key3}) {
                try { s3.deleteObject(DeleteObjectRequest.builder().bucket(bucket).key(key).build()); }
                catch (Exception ignored) {}
            }
        }
    }

    @Test
    @Order(6)
    void testCopyObject() {
        String srcKey = testKey("copy-src");
        String dstKey = testKey("copy-dst");
        String content = "CopyObject test content";

        try {
            s3.putObject(PutObjectRequest.builder().bucket(bucket).key(srcKey).build(),
                    RequestBody.fromString(content));

            s3.copyObject(CopyObjectRequest.builder()
                    .sourceBucket(bucket).sourceKey(srcKey)
                    .destinationBucket(bucket).destinationKey(dstKey)
                    .build());

            var getResp = s3.getObjectAsBytes(GetObjectRequest.builder().bucket(bucket).key(dstKey).build());
            assertEquals(content, getResp.asUtf8String());
        } finally {
            try { s3.deleteObject(DeleteObjectRequest.builder().bucket(bucket).key(srcKey).build()); } catch (Exception ignored) {}
            try { s3.deleteObject(DeleteObjectRequest.builder().bucket(bucket).key(dstKey).build()); } catch (Exception ignored) {}
        }
    }

    @Test
    @Order(7)
    void testAbortMultipartUpload() {
        String key = testKey("abort-multipart");

        try {
            var create = s3.createMultipartUpload(CreateMultipartUploadRequest.builder()
                    .bucket(bucket).key(key).build());
            String uploadId = create.uploadId();

            byte[] partData = new byte[5 * 1024 * 1024];
            java.util.Arrays.fill(partData, (byte) 'B');
            s3.uploadPart(UploadPartRequest.builder()
                    .bucket(bucket).key(key).uploadId(uploadId).partNumber(1)
                    .contentLength((long) partData.length).build(), RequestBody.fromBytes(partData));

            s3.abortMultipartUpload(AbortMultipartUploadRequest.builder()
                    .bucket(bucket).key(key).uploadId(uploadId).build());

            // ListParts should fail after abort
            assertThrows(Exception.class, () ->
                    s3.listParts(ListPartsRequest.builder()
                            .bucket(bucket).key(key).uploadId(uploadId).build()));
        } finally {
            try { s3.deleteObject(DeleteObjectRequest.builder().bucket(bucket).key(key).build()); } catch (Exception ignored) {}
        }
    }

    @Test
    @Order(8)
    void testRestoreObject() {
        String key = testKey("restore");

        try {
            s3.putObject(PutObjectRequest.builder()
                            .bucket(bucket).key(key)
                            .storageClass(StorageClass.GLACIER)
                            .build(),
                    RequestBody.fromString("restore test content"));

            // RestoreObject — proxy returns synthetic 200/202
            s3.restoreObject(RestoreObjectRequest.builder()
                    .bucket(bucket).key(key)
                    .restoreRequest(software.amazon.awssdk.services.s3.model.RestoreRequest.builder()
                            .days(7).build())
                    .build());
        } finally {
            try { s3.deleteObject(DeleteObjectRequest.builder().bucket(bucket).key(key).build()); } catch (Exception ignored) {}
        }
    }

    // ---- Control Plane ----

    @Test
    @Order(10)
    void testLifecycleCRUD() {
        try {
            s3.putBucketLifecycleConfiguration(PutBucketLifecycleConfigurationRequest.builder()
                    .bucket(bucket)
                    .lifecycleConfiguration(BucketLifecycleConfiguration.builder()
                            .rules(LifecycleRule.builder()
                                    .id("JavaV2-Exp")
                                    .status(ExpirationStatus.ENABLED)
                                    .filter(LifecycleRuleFilter.builder().prefix("javav2-test/").build())
                                    .expiration(LifecycleExpiration.builder().days(365).build())
                                    .build())
                            .build())
                    .build());

            var got = s3.getBucketLifecycleConfiguration(GetBucketLifecycleConfigurationRequest.builder()
                    .bucket(bucket).build());
            assertFalse(got.rules().isEmpty());

            s3.deleteBucketLifecycle(DeleteBucketLifecycleRequest.builder().bucket(bucket).build());
        } catch (Exception e) {
            try { s3.deleteBucketLifecycle(DeleteBucketLifecycleRequest.builder().bucket(bucket).build()); }
            catch (Exception ignored) {}
            throw e;
        }
    }

    @Test
    @Order(11)
    void testCORSCRUD() {
        try {
            s3.putBucketCors(PutBucketCorsRequest.builder()
                    .bucket(bucket)
                    .corsConfiguration(CORSConfiguration.builder()
                            .corsRules(CORSRule.builder()
                                    .allowedMethods("GET", "PUT")
                                    .allowedOrigins("https://javav2-test.example.com")
                                    .allowedHeaders("Authorization")
                                    .maxAgeSeconds(3600)
                                    .build())
                            .build())
                    .build());

            var got = s3.getBucketCors(GetBucketCorsRequest.builder().bucket(bucket).build());
            assertFalse(got.corsRules().isEmpty());

            s3.deleteBucketCors(DeleteBucketCorsRequest.builder().bucket(bucket).build());
        } catch (Exception e) {
            try { s3.deleteBucketCors(DeleteBucketCorsRequest.builder().bucket(bucket).build()); }
            catch (Exception ignored) {}
            throw e;
        }
    }

    @Test
    @Order(12)
    void testLoggingCRUD() {
        try {
            s3.putBucketLogging(PutBucketLoggingRequest.builder()
                    .bucket(bucket)
                    .bucketLoggingStatus(BucketLoggingStatus.builder()
                            .loggingEnabled(LoggingEnabled.builder()
                                    .targetBucket(bucket)
                                    .targetPrefix("javav2-logs/")
                                    .build())
                            .build())
                    .build());

            var got = s3.getBucketLogging(GetBucketLoggingRequest.builder().bucket(bucket).build());
            assertNotNull(got.loggingEnabled());

            // Clear logging
            s3.putBucketLogging(PutBucketLoggingRequest.builder()
                    .bucket(bucket)
                    .bucketLoggingStatus(BucketLoggingStatus.builder().build())
                    .build());
        } catch (Exception e) {
            try {
                s3.putBucketLogging(PutBucketLoggingRequest.builder()
                        .bucket(bucket)
                        .bucketLoggingStatus(BucketLoggingStatus.builder().build())
                        .build());
            } catch (Exception ignored) {}
            throw e;
        }
    }

    @Test
    @Order(13)
    void testWebsiteCRUD() {
        try {
            s3.putBucketWebsite(PutBucketWebsiteRequest.builder()
                    .bucket(bucket)
                    .websiteConfiguration(WebsiteConfiguration.builder()
                            .indexDocument(IndexDocument.builder().suffix("index.html").build())
                            .errorDocument(ErrorDocument.builder().key("error.html").build())
                            .build())
                    .build());

            var got = s3.getBucketWebsite(GetBucketWebsiteRequest.builder().bucket(bucket).build());
            assertEquals("index.html", got.indexDocument().suffix());

            s3.deleteBucketWebsite(DeleteBucketWebsiteRequest.builder().bucket(bucket).build());
        } catch (Exception e) {
            try { s3.deleteBucketWebsite(DeleteBucketWebsiteRequest.builder().bucket(bucket).build()); }
            catch (Exception ignored) {}
            throw e;
        }
    }

    @Test
    @Order(14)
    void testTaggingCRUD() {
        String key = testKey("tagging");
        try {
            s3.putObject(PutObjectRequest.builder().bucket(bucket).key(key).build(),
                    RequestBody.fromString("tag test"));

            s3.putObjectTagging(PutObjectTaggingRequest.builder()
                    .bucket(bucket).key(key)
                    .tagging(Tagging.builder().tagSet(
                            software.amazon.awssdk.services.s3.model.Tag.builder().key("Env").value("JavaV2Test").build()
                    ).build()).build());

            var got = s3.getObjectTagging(GetObjectTaggingRequest.builder()
                    .bucket(bucket).key(key).build());
            assertFalse(got.tagSet().isEmpty());

            s3.deleteObjectTagging(DeleteObjectTaggingRequest.builder()
                    .bucket(bucket).key(key).build());
        } finally {
            s3.deleteObject(DeleteObjectRequest.builder().bucket(bucket).key(key).build());
        }
    }
}
