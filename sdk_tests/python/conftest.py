"""Shared fixtures for Python SDK tests."""
import os
import time

# Force boto3 to use a plain sha256 payload (no trailer checksum, no
# chunked body) so the GCS XML API can verify the re-signed request
# directly. Must be set BEFORE botocore is imported.
os.environ.setdefault("AWS_REQUEST_CHECKSUM_CALCULATION", "when_required")
os.environ.setdefault("AWS_RESPONSE_CHECKSUM_VALIDATION", "when_required")

import boto3
import pytest
from botocore.config import Config


def _require_env(name: str) -> str:
    val = os.environ.get(name, "")
    if not val:
        pytest.fail(f"Missing required env var: {name}")
    return val


@pytest.fixture(scope="session")
def env():
    """Load and validate environment variables."""
    return {
        "proxy_endpoint": _require_env("PROXY_ENDPOINT"),
        "hmac_access": _require_env("GCS_HMAC_ACCESS"),
        "hmac_secret": _require_env("GCS_HMAC_SECRET"),
        "test_bucket": _require_env("TEST_BUCKET"),
        "test_prefix": os.environ.get("TEST_PREFIX", ""),
    }


@pytest.fixture(scope="session")
def s3_client(env):
    """Create an S3 client pointing at the proxy."""
    return boto3.client(
        "s3",
        endpoint_url=env["proxy_endpoint"],
        aws_access_key_id=env["hmac_access"],
        aws_secret_access_key=env["hmac_secret"],
        region_name="us-east-1",
        config=Config(
            s3={"addressing_style": "path"},
            retries={"max_attempts": 3, "mode": "standard"},
        ),
    )


@pytest.fixture(scope="session")
def bucket(env):
    return env["test_bucket"]


@pytest.fixture()
def test_key(env):
    """Generate a unique test key."""
    prefix = env["test_prefix"]
    ts = int(time.time() * 1_000_000)
    return f"{prefix}py-test-{ts}"
