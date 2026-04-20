"""Data plane tests using Python boto3 SDK."""
import io
import time

import pytest


class TestObjectCRUD:
    def test_put_get_delete(self, s3_client, bucket, test_key):
        content = b"Hello from Python boto3 test!"

        # PutObject
        s3_client.put_object(Bucket=bucket, Key=test_key, Body=content)

        # GetObject
        resp = s3_client.get_object(Bucket=bucket, Key=test_key)
        body = resp["Body"].read()
        assert body == content, f"Body mismatch: {body!r}"

        # HeadObject
        head = s3_client.head_object(Bucket=bucket, Key=test_key)
        assert head["ContentLength"] == len(content)

        # DeleteObject
        s3_client.delete_object(Bucket=bucket, Key=test_key)

        # GetObject after delete should fail
        with pytest.raises(s3_client.exceptions.NoSuchKey):
            s3_client.get_object(Bucket=bucket, Key=test_key)


class TestMultipartUpload:
    def test_multipart(self, s3_client, bucket, test_key):
        part1 = b"A" * (5 * 1024 * 1024)  # 5MB minimum
        part2 = b"Final part"

        # Create
        create = s3_client.create_multipart_upload(Bucket=bucket, Key=test_key)
        upload_id = create["UploadId"]

        try:
            # Upload parts
            up1 = s3_client.upload_part(
                Bucket=bucket, Key=test_key, UploadId=upload_id,
                PartNumber=1, Body=part1,
            )
            up2 = s3_client.upload_part(
                Bucket=bucket, Key=test_key, UploadId=upload_id,
                PartNumber=2, Body=part2,
            )

            # Complete
            s3_client.complete_multipart_upload(
                Bucket=bucket, Key=test_key, UploadId=upload_id,
                MultipartUpload={
                    "Parts": [
                        {"PartNumber": 1, "ETag": up1["ETag"]},
                        {"PartNumber": 2, "ETag": up2["ETag"]},
                    ]
                },
            )

            # Verify
            resp = s3_client.get_object(Bucket=bucket, Key=test_key)
            merged = resp["Body"].read()
            assert len(merged) == len(part1) + len(part2)
        finally:
            s3_client.delete_object(Bucket=bucket, Key=test_key)


class TestListObjects:
    def test_list_objects_v2(self, s3_client, bucket, env):
        prefix = env["test_prefix"] + "py-list/"
        keys = [prefix + x for x in ("a", "b", "c")]

        try:
            for key in keys:
                s3_client.put_object(Bucket=bucket, Key=key, Body=b"list test")

            resp = s3_client.list_objects_v2(Bucket=bucket, Prefix=prefix)
            contents = resp.get("Contents", [])
            assert len(contents) >= 3, f"Expected >=3, got {len(contents)}"
        finally:
            for key in keys:
                s3_client.delete_object(Bucket=bucket, Key=key)


class TestDeleteObjects:
    def test_delete_objects(self, s3_client, bucket, env):
        prefix = env["test_prefix"] + "py-delobj/"
        key1 = prefix + "obj1-" + str(int(time.time() * 1000))
        key2 = prefix + "obj2-" + str(int(time.time() * 1000))
        key3 = prefix + "obj3-" + str(int(time.time() * 1000))

        try:
            # Create 3 objects
            for key in (key1, key2, key3):
                s3_client.put_object(Bucket=bucket, Key=key, Body=b"delete-objects test")

            # DeleteObjects — bulk delete key1 and key2
            resp = s3_client.delete_objects(
                Bucket=bucket,
                Delete={
                    "Objects": [{"Key": key1}, {"Key": key2}],
                    "Quiet": False,
                },
            )
            deleted = resp.get("Deleted", [])
            errors = resp.get("Errors", [])
            assert len(errors) == 0, f"DeleteObjects errors: {errors}"
            assert len(deleted) == 2, f"Expected 2 deleted, got {len(deleted)}"

            # Verify key1 and key2 are gone
            for key in (key1, key2):
                with pytest.raises(s3_client.exceptions.ClientError):
                    s3_client.head_object(Bucket=bucket, Key=key)

            # Verify key3 still exists
            head = s3_client.head_object(Bucket=bucket, Key=key3)
            assert head["ResponseMetadata"]["HTTPStatusCode"] == 200
        finally:
            # Cleanup remaining
            for key in (key1, key2, key3):
                try:
                    s3_client.delete_object(Bucket=bucket, Key=key)
                except Exception:
                    pass


class TestStorageClass:
    def test_storage_class_translation(self, s3_client, bucket, test_key):
        try:
            s3_client.put_object(
                Bucket=bucket, Key=test_key,
                Body=b"storage class test",
                StorageClass="STANDARD_IA",
            )
            head = s3_client.head_object(Bucket=bucket, Key=test_key)
            # Just verify request succeeded; exact class depends on GCS mapping
            assert head["ResponseMetadata"]["HTTPStatusCode"] == 200
        finally:
            s3_client.delete_object(Bucket=bucket, Key=test_key)


class TestCopyObject:
    def test_copy_object(self, s3_client, bucket, env):
        prefix = env["test_prefix"]
        ts = int(time.time() * 1_000_000)
        src_key = f"{prefix}py-copy-src-{ts}"
        dst_key = f"{prefix}py-copy-dst-{ts}"
        content = b"CopyObject test content"

        try:
            s3_client.put_object(Bucket=bucket, Key=src_key, Body=content)

            s3_client.copy_object(
                Bucket=bucket,
                Key=dst_key,
                CopySource=f"{bucket}/{src_key}",
            )

            resp = s3_client.get_object(Bucket=bucket, Key=dst_key)
            body = resp["Body"].read()
            assert body == content, f"Copy body mismatch: {body!r}"
        finally:
            for key in (src_key, dst_key):
                try:
                    s3_client.delete_object(Bucket=bucket, Key=key)
                except Exception:
                    pass


class TestAbortMultipartUpload:
    def test_abort_multipart(self, s3_client, bucket, env):
        prefix = env["test_prefix"]
        ts = int(time.time() * 1_000_000)
        key = f"{prefix}py-abort-multipart-{ts}"

        try:
            create = s3_client.create_multipart_upload(Bucket=bucket, Key=key)
            upload_id = create["UploadId"]

            s3_client.upload_part(
                Bucket=bucket, Key=key, UploadId=upload_id,
                PartNumber=1, Body=b"A" * (5 * 1024 * 1024),
            )

            s3_client.abort_multipart_upload(
                Bucket=bucket, Key=key, UploadId=upload_id,
            )

            # ListParts should fail after abort
            with pytest.raises(s3_client.exceptions.ClientError):
                s3_client.list_parts(
                    Bucket=bucket, Key=key, UploadId=upload_id,
                )
        finally:
            try:
                s3_client.delete_object(Bucket=bucket, Key=key)
            except Exception:
                pass


class TestRestoreObject:
    def test_restore_object(self, s3_client, bucket, env):
        prefix = env["test_prefix"]
        ts = int(time.time() * 1_000_000)
        key = f"{prefix}py-restore-{ts}"

        try:
            s3_client.put_object(
                Bucket=bucket, Key=key,
                Body=b"restore test content",
                StorageClass="GLACIER",
            )

            # RestoreObject — proxy returns synthetic 200/202
            resp = s3_client.restore_object(
                Bucket=bucket, Key=key,
                RestoreRequest={"Days": 7},
            )
            status = resp["ResponseMetadata"]["HTTPStatusCode"]
            assert status in (200, 202), f"Expected 200/202, got {status}"
        finally:
            try:
                s3_client.delete_object(Bucket=bucket, Key=key)
            except Exception:
                pass

