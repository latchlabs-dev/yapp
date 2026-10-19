"""Check the actual GCS adapter's immutable-generation and integrity contract."""
import base64
import hashlib
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

from google.api_core.exceptions import PreconditionFailed
import google_crc32c

from session_query.config import Config
from session_query.storage import GCS


class GCSTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.data = b"immutable archived bytes"
        self.sha = hashlib.sha256(self.data).hexdigest()
        self.blob = Mock(size=len(self.data), generation=42, content_encoding=None,
                         crc32c=base64.b64encode(google_crc32c.value(self.data).to_bytes(4, "big")).decode())
        self.blob.download_to_filename.side_effect = lambda path, **kw: Path(path).write_bytes(self.data)
        self.bucket = Mock()
        self.bucket.get_blob.return_value = self.bucket.blob.return_value = self.blob
        self.patch = patch("session_query.storage.storage.Client")
        self.patch.start().return_value.bucket.return_value = self.bucket
        self.gcs = GCS(Config(root=self.root, token="x" * 32, bucket="test-bucket"))

    def tearDown(self):
        self.patch.stop()
        self.temp.cleanup()

    def test_download_pins_generation_and_checks_crc_size_and_sha(self):
        destination = self.root / "download"
        proof = self.gcs.download("source", destination, len(self.data), generation=42)
        self.assertEqual((42, self.sha, len(self.data)), (proof["generation"], proof["sha256"], proof["size"]))
        self.bucket.get_blob.assert_called_once_with("source", generation=42, timeout=20)
        self.blob.download_to_filename.assert_called_once_with(str(destination), if_generation_match=42,
                                                               checksum="crc32c", timeout=30)
        self.assertEqual(0o600, destination.stat().st_mode & 0o777)
        self.blob.crc32c = "AAAAAA=="
        with self.assertRaisesRegex(ValueError, "integrity mismatch"):
            self.gcs.download("source", destination, len(self.data))
        with self.assertRaisesRegex(ValueError, "oversized"):
            self.gcs.download("source", destination, 1)

    def test_existing_immutable_object_is_reloaded_and_durable_copy_verified(self):
        source = self.root / "chunk.parquet"
        source.write_bytes(self.data)
        self.blob.upload_from_filename.side_effect = PreconditionFailed("already exists")
        self.blob.reload.side_effect = lambda **kw: setattr(self.blob, "metadata", {"sha256": self.sha})
        proof = self.gcs.put_immutable("derived/chunk", source, "application/vnd.apache.parquet")
        self.assertEqual(self.sha, proof["sha256"])
        self.blob.upload_from_filename.assert_called_once_with(str(source), content_type="application/vnd.apache.parquet",
                                                              if_generation_match=0, checksum="crc32c", timeout=30)
        self.bucket.get_blob.assert_called_once_with("derived/chunk", generation=42, timeout=20)
        self.assertFalse(source.with_suffix(".verify").exists())
        self.blob.reload.side_effect = lambda **kw: setattr(self.blob, "metadata", {"sha256": "0" * 64})
        with self.assertRaisesRegex(ValueError, "immutable object conflict"):
            self.gcs.put_immutable("derived/chunk", source, "application/vnd.apache.parquet")
        self.blob.reload.side_effect = lambda **kw: setattr(self.blob, "metadata", {"sha256": self.sha})
        self.data = b"different archived bytes"  # Same length; CRC is updated as the provider would do.
        self.blob.crc32c = base64.b64encode(google_crc32c.value(self.data).to_bytes(4, "big")).decode()
        with self.assertRaisesRegex(ValueError, "durable Parquet differs"):
            self.gcs.put_immutable("derived/chunk", source, "application/vnd.apache.parquet")
        self.assertFalse(source.with_suffix(".verify").exists())
