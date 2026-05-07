/**
 * S3Proxy functional tests using AWS SDK for C++ (1.11.765).
 */
#include <gtest/gtest.h>

#include <aws/core/Aws.h>
#include <aws/core/auth/AWSCredentials.h>
#include <aws/core/client/ClientConfiguration.h>
#include <aws/s3/S3Client.h>
#include <aws/s3/model/PutObjectRequest.h>
#include <aws/s3/model/GetObjectRequest.h>
#include <aws/s3/model/HeadObjectRequest.h>
#include <aws/s3/model/DeleteObjectRequest.h>
#include <aws/s3/model/DeleteObjectsRequest.h>
#include <aws/s3/model/Delete.h>
#include <aws/s3/model/ObjectIdentifier.h>
#include <aws/s3/model/ListObjectsV2Request.h>
#include <aws/s3/model/CreateMultipartUploadRequest.h>
#include <aws/s3/model/UploadPartRequest.h>
#include <aws/s3/model/CompleteMultipartUploadRequest.h>
#include <aws/s3/model/CompletedMultipartUpload.h>
#include <aws/s3/model/CompletedPart.h>
#include <aws/s3/model/AbortMultipartUploadRequest.h>
#include <aws/s3/model/ListPartsRequest.h>
#include <aws/s3/model/CopyObjectRequest.h>
#include <aws/s3/model/RestoreObjectRequest.h>
#include <aws/s3/model/PutBucketLifecycleConfigurationRequest.h>
#include <aws/s3/model/GetBucketLifecycleConfigurationRequest.h>
#include <aws/s3/model/DeleteBucketLifecycleRequest.h>
#include <aws/s3/model/PutBucketCorsRequest.h>
#include <aws/s3/model/GetBucketCorsRequest.h>
#include <aws/s3/model/DeleteBucketCorsRequest.h>
#include <aws/s3/model/PutBucketLoggingRequest.h>
#include <aws/s3/model/GetBucketLoggingRequest.h>
#include <aws/s3/model/PutBucketWebsiteRequest.h>
#include <aws/s3/model/GetBucketWebsiteRequest.h>
#include <aws/s3/model/DeleteBucketWebsiteRequest.h>
#include <aws/s3/model/PutObjectTaggingRequest.h>
#include <aws/s3/model/GetObjectTaggingRequest.h>
#include <aws/s3/model/DeleteObjectTaggingRequest.h>

#include <chrono>
#include <cstdlib>
#include <memory>
#include <sstream>
#include <string>

namespace {

std::string GetEnvOrFail(const char* name) {
    const char* val = std::getenv(name);
    if (!val || std::string(val).empty()) {
        throw std::runtime_error(std::string("Missing env var: ") + name);
    }
    return std::string(val);
}

std::string GetEnvOrDefault(const char* name, const std::string& def = "") {
    const char* val = std::getenv(name);
    return (val && std::string(val).length() > 0) ? std::string(val) : def;
}

std::string MakeTestKey(const std::string& prefix, const std::string& suffix) {
    auto now = std::chrono::system_clock::now().time_since_epoch();
    auto ns = std::chrono::duration_cast<std::chrono::nanoseconds>(now).count();
    return prefix + "cpp-" + suffix + "-" + std::to_string(ns);
}

// kSdkTestBucket is the default GCS bucket for C++ SDK compatibility tests.
// Override via TEST_BUCKET env var for different environments (e.g. production).
static const char* kSdkTestBucket = "s3proxy-sdk-cpp";

class S3ProxyTest : public ::testing::Test {
protected:
    static std::shared_ptr<Aws::S3::S3Client> s3;
    static std::string bucket;
    static std::string prefix;
    static Aws::SDKOptions options;

    static void SetUpTestSuite() {
        Aws::InitAPI(options);

        std::string endpoint = GetEnvOrFail("PROXY_ENDPOINT");
        std::string access = GetEnvOrFail("GCS_HMAC_ACCESS");
        std::string secret = GetEnvOrFail("GCS_HMAC_SECRET");
        bucket = GetEnvOrDefault("TEST_BUCKET", kSdkTestBucket);
        prefix = GetEnvOrDefault("TEST_PREFIX");

        Aws::Client::ClientConfiguration config;
        config.endpointOverride = endpoint;
        config.region = "us-east-1";
        config.scheme = Aws::Http::Scheme::HTTP;
        config.verifySSL = false;

        Aws::Auth::AWSCredentials creds(access, secret);

        s3 = std::make_shared<Aws::S3::S3Client>(
            creds, config,
            // Recommended: proxy re-signs with UNSIGNED-PAYLOAD, this avoids
            // redundant SHA-256 computation on the client side.
            Aws::Client::AWSAuthV4Signer::PayloadSigningPolicy::Never,
            /* useVirtualAddressing */ false);
    }

    static void TearDownTestSuite() {
        s3.reset();
        Aws::ShutdownAPI(options);
    }

    void DeleteKey(const std::string& key) {
        Aws::S3::Model::DeleteObjectRequest req;
        req.SetBucket(bucket);
        req.SetKey(key);
        s3->DeleteObject(req);
    }
};

std::shared_ptr<Aws::S3::S3Client> S3ProxyTest::s3;
std::string S3ProxyTest::bucket;
std::string S3ProxyTest::prefix;
Aws::SDKOptions S3ProxyTest::options;

// ---- Data Plane ----

TEST_F(S3ProxyTest, ObjectCRUD) {
    auto key = MakeTestKey(prefix, "crud");
    std::string content = "Hello from C++ SDK!";

    // Put
    Aws::S3::Model::PutObjectRequest putReq;
    putReq.SetBucket(bucket);
    putReq.SetKey(key);
    auto body = Aws::MakeShared<Aws::StringStream>("put");
    *body << content;
    putReq.SetBody(body);
    auto putResult = s3->PutObject(putReq);
    ASSERT_TRUE(putResult.IsSuccess()) << putResult.GetError().GetMessage();

    // Get
    Aws::S3::Model::GetObjectRequest getReq;
    getReq.SetBucket(bucket);
    getReq.SetKey(key);
    auto getResult = s3->GetObject(getReq);
    ASSERT_TRUE(getResult.IsSuccess()) << getResult.GetError().GetMessage();
    std::stringstream ss;
    ss << getResult.GetResult().GetBody().rdbuf();
    ASSERT_EQ(ss.str(), content);

    // Head
    Aws::S3::Model::HeadObjectRequest headReq;
    headReq.SetBucket(bucket);
    headReq.SetKey(key);
    auto headResult = s3->HeadObject(headReq);
    ASSERT_TRUE(headResult.IsSuccess());
    ASSERT_GT(headResult.GetResult().GetContentLength(), 0);

    // Delete
    DeleteKey(key);

    // Get after delete - should fail
    auto getAfter = s3->GetObject(getReq);
    ASSERT_FALSE(getAfter.IsSuccess());
}

TEST_F(S3ProxyTest, ListObjectsV2) {
    std::string listPrefix = prefix + "cpp-list/";
    std::vector<std::string> keys = {listPrefix + "a", listPrefix + "b", listPrefix + "c"};

    for (const auto& key : keys) {
        Aws::S3::Model::PutObjectRequest req;
        req.SetBucket(bucket);
        req.SetKey(key);
        auto body = Aws::MakeShared<Aws::StringStream>("put");
        *body << "list test";
        req.SetBody(body);
        ASSERT_TRUE(s3->PutObject(req).IsSuccess());
    }

    Aws::S3::Model::ListObjectsV2Request listReq;
    listReq.SetBucket(bucket);
    listReq.SetPrefix(listPrefix);
    auto listResult = s3->ListObjectsV2(listReq);
    ASSERT_TRUE(listResult.IsSuccess());
    ASSERT_GE(listResult.GetResult().GetContents().size(), 3u);

    for (const auto& key : keys) {
        DeleteKey(key);
    }
}

TEST_F(S3ProxyTest, MultipartUpload) {
    auto key = MakeTestKey(prefix, "multipart");
    std::string part1(5 * 1024 * 1024, 'A'); // 5MB minimum
    std::string part2 = "Final part";

    Aws::S3::Model::CreateMultipartUploadRequest createReq;
    createReq.SetBucket(bucket);
    createReq.SetKey(key);
    auto createResult = s3->CreateMultipartUpload(createReq);
    ASSERT_TRUE(createResult.IsSuccess()) << createResult.GetError().GetMessage();
    auto uploadId = createResult.GetResult().GetUploadId();

    // Upload Part 1
    Aws::S3::Model::UploadPartRequest up1Req;
    up1Req.SetBucket(bucket);
    up1Req.SetKey(key);
    up1Req.SetUploadId(uploadId);
    up1Req.SetPartNumber(1);
    up1Req.SetContentLength(part1.size());
    auto body1 = Aws::MakeShared<Aws::StringStream>("up1");
    *body1 << part1;
    up1Req.SetBody(body1);
    auto up1Result = s3->UploadPart(up1Req);
    ASSERT_TRUE(up1Result.IsSuccess()) << up1Result.GetError().GetMessage();

    // Upload Part 2
    Aws::S3::Model::UploadPartRequest up2Req;
    up2Req.SetBucket(bucket);
    up2Req.SetKey(key);
    up2Req.SetUploadId(uploadId);
    up2Req.SetPartNumber(2);
    up2Req.SetContentLength(part2.size());
    auto body2 = Aws::MakeShared<Aws::StringStream>("up2");
    *body2 << part2;
    up2Req.SetBody(body2);
    auto up2Result = s3->UploadPart(up2Req);
    ASSERT_TRUE(up2Result.IsSuccess()) << up2Result.GetError().GetMessage();

    // Complete
    Aws::S3::Model::CompletedPart cp1;
    cp1.SetPartNumber(1);
    cp1.SetETag(up1Result.GetResult().GetETag());
    Aws::S3::Model::CompletedPart cp2;
    cp2.SetPartNumber(2);
    cp2.SetETag(up2Result.GetResult().GetETag());

    Aws::S3::Model::CompletedMultipartUpload completed;
    completed.AddParts(cp1);
    completed.AddParts(cp2);

    Aws::S3::Model::CompleteMultipartUploadRequest completeReq;
    completeReq.SetBucket(bucket);
    completeReq.SetKey(key);
    completeReq.SetUploadId(uploadId);
    completeReq.SetMultipartUpload(completed);
    auto completeResult = s3->CompleteMultipartUpload(completeReq);
    ASSERT_TRUE(completeResult.IsSuccess()) << completeResult.GetError().GetMessage();

    // Verify merged size
    Aws::S3::Model::HeadObjectRequest headReq;
    headReq.SetBucket(bucket);
    headReq.SetKey(key);
    auto headResult = s3->HeadObject(headReq);
    ASSERT_TRUE(headResult.IsSuccess());
    ASSERT_EQ(headResult.GetResult().GetContentLength(),
              static_cast<long long>(part1.size() + part2.size()));

    DeleteKey(key);
}

TEST_F(S3ProxyTest, DeleteObjects) {
    auto key1 = MakeTestKey(prefix, "delobj-1");
    auto key2 = MakeTestKey(prefix, "delobj-2");
    auto key3 = MakeTestKey(prefix, "delobj-3");

    // Create 3 objects
    for (const auto& key : {key1, key2, key3}) {
        Aws::S3::Model::PutObjectRequest putReq;
        putReq.SetBucket(bucket);
        putReq.SetKey(key);
        auto body = Aws::MakeShared<Aws::StringStream>("put");
        *body << "delete-objects test";
        putReq.SetBody(body);
        ASSERT_TRUE(s3->PutObject(putReq).IsSuccess());
    }

    // DeleteObjects — bulk delete key1 and key2
    Aws::S3::Model::ObjectIdentifier id1;
    id1.SetKey(key1);
    Aws::S3::Model::ObjectIdentifier id2;
    id2.SetKey(key2);

    Aws::S3::Model::Delete delObj;
    delObj.AddObjects(id1);
    delObj.AddObjects(id2);
    delObj.SetQuiet(false);

    Aws::S3::Model::DeleteObjectsRequest delReq;
    delReq.SetBucket(bucket);
    delReq.SetDelete(delObj);
    auto delResult = s3->DeleteObjects(delReq);
    ASSERT_TRUE(delResult.IsSuccess()) << delResult.GetError().GetMessage();
    ASSERT_EQ(delResult.GetResult().GetDeleted().size(), 2u);
    ASSERT_TRUE(delResult.GetResult().GetErrors().empty());

    // Verify key1 and key2 are gone
    for (const auto& key : {key1, key2}) {
        Aws::S3::Model::HeadObjectRequest headReq;
        headReq.SetBucket(bucket);
        headReq.SetKey(key);
        ASSERT_FALSE(s3->HeadObject(headReq).IsSuccess());
    }

    // Verify key3 still exists
    Aws::S3::Model::HeadObjectRequest headReq;
    headReq.SetBucket(bucket);
    headReq.SetKey(key3);
    ASSERT_TRUE(s3->HeadObject(headReq).IsSuccess());

    DeleteKey(key3);
}

TEST_F(S3ProxyTest, StorageClass) {
    auto key = MakeTestKey(prefix, "storageclass");

    Aws::S3::Model::PutObjectRequest putReq;
    putReq.SetBucket(bucket);
    putReq.SetKey(key);
    putReq.SetStorageClass(Aws::S3::Model::StorageClass::STANDARD_IA);
    auto body = Aws::MakeShared<Aws::StringStream>("put");
    *body << "storage class test";
    putReq.SetBody(body);
    auto putResult = s3->PutObject(putReq);
    ASSERT_TRUE(putResult.IsSuccess()) << putResult.GetError().GetMessage();

    Aws::S3::Model::HeadObjectRequest headReq;
    headReq.SetBucket(bucket);
    headReq.SetKey(key);
    auto headResult = s3->HeadObject(headReq);
    ASSERT_TRUE(headResult.IsSuccess());

    DeleteKey(key);
}

TEST_F(S3ProxyTest, CopyObject) {
    auto srcKey = MakeTestKey(prefix, "copy-src");
    auto dstKey = MakeTestKey(prefix, "copy-dst");
    std::string content = "CopyObject test content";

    // Put source
    Aws::S3::Model::PutObjectRequest putReq;
    putReq.SetBucket(bucket);
    putReq.SetKey(srcKey);
    auto body = Aws::MakeShared<Aws::StringStream>("put");
    *body << content;
    putReq.SetBody(body);
    ASSERT_TRUE(s3->PutObject(putReq).IsSuccess());

    // CopyObject
    Aws::S3::Model::CopyObjectRequest copyReq;
    copyReq.SetBucket(bucket);
    copyReq.SetKey(dstKey);
    copyReq.SetCopySource(bucket + "/" + srcKey);
    auto copyResult = s3->CopyObject(copyReq);
    ASSERT_TRUE(copyResult.IsSuccess()) << copyResult.GetError().GetMessage();

    // Verify destination content
    Aws::S3::Model::GetObjectRequest getReq;
    getReq.SetBucket(bucket);
    getReq.SetKey(dstKey);
    auto getResult = s3->GetObject(getReq);
    ASSERT_TRUE(getResult.IsSuccess());
    std::stringstream ss;
    ss << getResult.GetResult().GetBody().rdbuf();
    ASSERT_EQ(ss.str(), content);

    DeleteKey(srcKey);
    DeleteKey(dstKey);
}

TEST_F(S3ProxyTest, AbortMultipartUpload) {
    auto key = MakeTestKey(prefix, "abort-multipart");

    Aws::S3::Model::CreateMultipartUploadRequest createReq;
    createReq.SetBucket(bucket);
    createReq.SetKey(key);
    auto createResult = s3->CreateMultipartUpload(createReq);
    ASSERT_TRUE(createResult.IsSuccess()) << createResult.GetError().GetMessage();
    auto uploadId = createResult.GetResult().GetUploadId();

    // Upload a part
    std::string partData(5 * 1024 * 1024, 'B');
    Aws::S3::Model::UploadPartRequest upReq;
    upReq.SetBucket(bucket);
    upReq.SetKey(key);
    upReq.SetUploadId(uploadId);
    upReq.SetPartNumber(1);
    upReq.SetContentLength(partData.size());
    auto upBody = Aws::MakeShared<Aws::StringStream>("up");
    *upBody << partData;
    upReq.SetBody(upBody);
    ASSERT_TRUE(s3->UploadPart(upReq).IsSuccess());

    // Abort
    Aws::S3::Model::AbortMultipartUploadRequest abortReq;
    abortReq.SetBucket(bucket);
    abortReq.SetKey(key);
    abortReq.SetUploadId(uploadId);
    auto abortResult = s3->AbortMultipartUpload(abortReq);
    ASSERT_TRUE(abortResult.IsSuccess()) << abortResult.GetError().GetMessage();

    // ListParts should fail
    Aws::S3::Model::ListPartsRequest listReq;
    listReq.SetBucket(bucket);
    listReq.SetKey(key);
    listReq.SetUploadId(uploadId);
    ASSERT_FALSE(s3->ListParts(listReq).IsSuccess());

    DeleteKey(key);
}

TEST_F(S3ProxyTest, RestoreObject) {
    auto key = MakeTestKey(prefix, "restore");

    // Put with GLACIER storage class
    Aws::S3::Model::PutObjectRequest putReq;
    putReq.SetBucket(bucket);
    putReq.SetKey(key);
    putReq.SetStorageClass(Aws::S3::Model::StorageClass::GLACIER);
    auto body = Aws::MakeShared<Aws::StringStream>("put");
    *body << "restore test content";
    putReq.SetBody(body);
    ASSERT_TRUE(s3->PutObject(putReq).IsSuccess());

    // RestoreObject — proxy returns synthetic 200/202
    Aws::S3::Model::RestoreObjectRequest restoreReq;
    restoreReq.SetBucket(bucket);
    restoreReq.SetKey(key);
    Aws::S3::Model::RestoreRequest rr;
    rr.SetDays(7);
    restoreReq.SetRestoreRequest(rr);
    auto restoreResult = s3->RestoreObject(restoreReq);
    ASSERT_TRUE(restoreResult.IsSuccess()) << restoreResult.GetError().GetMessage();

    DeleteKey(key);
}

// ---- Control Plane ----

TEST_F(S3ProxyTest, LifecycleCRUD) {
    Aws::S3::Model::LifecycleRule rule;
    rule.SetID("CppTest-Exp");
    rule.SetStatus(Aws::S3::Model::ExpirationStatus::Enabled);
    Aws::S3::Model::LifecycleRuleFilter filter;
    filter.SetPrefix("cpp-test/");
    rule.SetFilter(filter);
    Aws::S3::Model::LifecycleExpiration exp;
    exp.SetDays(365);
    rule.SetExpiration(exp);

    Aws::S3::Model::BucketLifecycleConfiguration config;
    config.AddRules(rule);

    Aws::S3::Model::PutBucketLifecycleConfigurationRequest putReq;
    putReq.SetBucket(bucket);
    putReq.SetLifecycleConfiguration(config);
    auto putResult = s3->PutBucketLifecycleConfiguration(putReq);
    ASSERT_TRUE(putResult.IsSuccess()) << putResult.GetError().GetMessage();

    Aws::S3::Model::GetBucketLifecycleConfigurationRequest getReq;
    getReq.SetBucket(bucket);
    auto getResult = s3->GetBucketLifecycleConfiguration(getReq);
    ASSERT_TRUE(getResult.IsSuccess());
    ASSERT_FALSE(getResult.GetResult().GetRules().empty());

    Aws::S3::Model::DeleteBucketLifecycleRequest delReq;
    delReq.SetBucket(bucket);
    s3->DeleteBucketLifecycle(delReq);
}

TEST_F(S3ProxyTest, CORSCRUD) {
    Aws::S3::Model::CORSRule corsRule;
    corsRule.AddAllowedMethods("GET");
    corsRule.AddAllowedMethods("PUT");
    corsRule.AddAllowedOrigins("https://cpp-test.example.com");
    corsRule.AddAllowedHeaders("Authorization");
    corsRule.SetMaxAgeSeconds(3600);

    Aws::S3::Model::CORSConfiguration corsConfig;
    corsConfig.AddCORSRules(corsRule);

    Aws::S3::Model::PutBucketCorsRequest putReq;
    putReq.SetBucket(bucket);
    putReq.SetCORSConfiguration(corsConfig);
    auto putResult = s3->PutBucketCors(putReq);
    ASSERT_TRUE(putResult.IsSuccess()) << putResult.GetError().GetMessage();

    Aws::S3::Model::GetBucketCorsRequest getReq;
    getReq.SetBucket(bucket);
    auto getResult = s3->GetBucketCors(getReq);
    ASSERT_TRUE(getResult.IsSuccess());
    ASSERT_FALSE(getResult.GetResult().GetCORSRules().empty());

    Aws::S3::Model::DeleteBucketCorsRequest delReq;
    delReq.SetBucket(bucket);
    s3->DeleteBucketCors(delReq);
}

TEST_F(S3ProxyTest, TaggingCRUD) {
    auto key = MakeTestKey(prefix, "tagging");

    Aws::S3::Model::PutObjectRequest putObjReq;
    putObjReq.SetBucket(bucket);
    putObjReq.SetKey(key);
    auto body = Aws::MakeShared<Aws::StringStream>("put");
    *body << "tag test";
    putObjReq.SetBody(body);
    ASSERT_TRUE(s3->PutObject(putObjReq).IsSuccess());

    Aws::S3::Model::Tag tag;
    tag.SetKey("Env");
    tag.SetValue("CppTest");
    Aws::S3::Model::Tagging tagging;
    tagging.AddTagSet(tag);

    Aws::S3::Model::PutObjectTaggingRequest putTagReq;
    putTagReq.SetBucket(bucket);
    putTagReq.SetKey(key);
    putTagReq.SetTagging(tagging);
    ASSERT_TRUE(s3->PutObjectTagging(putTagReq).IsSuccess());

    Aws::S3::Model::GetObjectTaggingRequest getTagReq;
    getTagReq.SetBucket(bucket);
    getTagReq.SetKey(key);
    auto getTagResult = s3->GetObjectTagging(getTagReq);
    ASSERT_TRUE(getTagResult.IsSuccess());
    ASSERT_FALSE(getTagResult.GetResult().GetTagSet().empty());

    Aws::S3::Model::DeleteObjectTaggingRequest delTagReq;
    delTagReq.SetBucket(bucket);
    delTagReq.SetKey(key);
    s3->DeleteObjectTagging(delTagReq);

    DeleteKey(key);
}

TEST_F(S3ProxyTest, LoggingCRUD) {
    Aws::S3::Model::LoggingEnabled logging;
    logging.SetTargetBucket(bucket);
    logging.SetTargetPrefix("cpp-logs/");

    Aws::S3::Model::BucketLoggingStatus loggingStatus;
    loggingStatus.SetLoggingEnabled(logging);

    Aws::S3::Model::PutBucketLoggingRequest putReq;
    putReq.SetBucket(bucket);
    putReq.SetBucketLoggingStatus(loggingStatus);
    auto putResult = s3->PutBucketLogging(putReq);
    ASSERT_TRUE(putResult.IsSuccess()) << putResult.GetError().GetMessage();

    Aws::S3::Model::GetBucketLoggingRequest getReq;
    getReq.SetBucket(bucket);
    auto getResult = s3->GetBucketLogging(getReq);
    ASSERT_TRUE(getResult.IsSuccess());
    // LoggingEnabled should be set
    ASSERT_FALSE(getResult.GetResult().GetLoggingEnabled().GetTargetBucket().empty());

    // Clear logging
    Aws::S3::Model::PutBucketLoggingRequest clearReq;
    clearReq.SetBucket(bucket);
    clearReq.SetBucketLoggingStatus(Aws::S3::Model::BucketLoggingStatus());
    s3->PutBucketLogging(clearReq);
}

TEST_F(S3ProxyTest, WebsiteCRUD) {
    Aws::S3::Model::IndexDocument indexDoc;
    indexDoc.SetSuffix("index.html");
    Aws::S3::Model::ErrorDocument errorDoc;
    errorDoc.SetKey("error.html");

    Aws::S3::Model::WebsiteConfiguration websiteConfig;
    websiteConfig.SetIndexDocument(indexDoc);
    websiteConfig.SetErrorDocument(errorDoc);

    Aws::S3::Model::PutBucketWebsiteRequest putReq;
    putReq.SetBucket(bucket);
    putReq.SetWebsiteConfiguration(websiteConfig);
    auto putResult = s3->PutBucketWebsite(putReq);
    ASSERT_TRUE(putResult.IsSuccess()) << putResult.GetError().GetMessage();

    Aws::S3::Model::GetBucketWebsiteRequest getReq;
    getReq.SetBucket(bucket);
    auto getResult = s3->GetBucketWebsite(getReq);
    ASSERT_TRUE(getResult.IsSuccess());
    ASSERT_EQ(getResult.GetResult().GetIndexDocument().GetSuffix(), "index.html");

    Aws::S3::Model::DeleteBucketWebsiteRequest delReq;
    delReq.SetBucket(bucket);
    s3->DeleteBucketWebsite(delReq);
}

} // namespace
