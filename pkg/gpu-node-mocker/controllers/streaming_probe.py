# Copyright (c) KAITO authors.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Streamer probe for gpu-node-mocker shadow pods.

Validates the KAITO model-streaming path on CPU, before the inference simulator
starts, so that "mock serving is Ready" also means "streaming is configured
correctly".

Two modes:
  sas           fetch-sas has already minted a SAS; list the container over plain
                HTTPS and assert it holds *.safetensors. Nothing is streamed, so
                this mode needs neither torch nor the streamer.
  direct-azure  list via the streamer and read one tensor into CPU memory, which
                additionally proves bulk transfer works.

Listing is the load-bearing check in both modes. KAITO's own bootstrap does NOT
catch an empty container: a static ModelMirror is marked Ready without inspecting
it, and fetch_sas.py exits 0 when it finds no safetensors. This probe is the only
thing standing between an empty container and a GPU pod that fails to load.

Environment:
  PROBE_MODE                   "sas" or "direct-azure"
  PROBE_MODEL_URI              model URI, used when STREAM_MODEL_URI is unset
  STREAM_MODEL_URI             model URI exported by the sourced SAS env file
  AZURE_STORAGE_ACCOUNT_NAME   storage account, exported by fetch-sas
  AZURE_STORAGE_SAS_TOKEN      SAS token, exported by fetch-sas
  PROBE_TIMEOUT_SECONDS        wall-clock budget for the whole probe
  RUNAI_STREAMER_MEMORY_LIMIT  streamer CPU buffer size in bytes; 0 sizes it to
                               the largest tensor in the shard being read
"""

import os
import sys
import threading
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET

DEFAULT_TIMEOUT_SECONDS = 600


def _scrub(text: str) -> str:
    """Strip query strings from anything we print.

    A SAS token lives in the query string, so a bare URL in an error message
    would leak a live credential into the pod logs.
    """
    parts = urllib.parse.urlsplit(text)
    if not parts.scheme or not parts.query:
        return text
    return urllib.parse.urlunsplit(
        (parts.scheme, parts.netloc, parts.path, "REDACTED", "")
    )


def _fail(message: str) -> int:
    print(f"ERROR: {_scrub(message)}", file=sys.stderr)
    return 1


def _start_watchdog(seconds: int) -> None:
    """Abort the process if the probe outruns its budget.

    A daemon thread calling os._exit rather than signal.alarm: Python only runs
    signal handlers between bytecodes, so SIGALRM is not reliably delivered
    while the main thread sits inside the streamer's C++ extension. os._exit
    also skips interpreter shutdown, which would otherwise block on those same
    threads.
    """

    def _abort() -> None:
        print(
            f"ERROR: probe exceeded {seconds}s budget; aborting",
            file=sys.stderr,
            flush=True,
        )
        os._exit(1)

    timer = threading.Timer(seconds, _abort)
    timer.daemon = True
    timer.start()


def _list_blobs(account: str, container: str, prefix: str, sas: str) -> list:
    """List blob names under a prefix via the blob REST API.

    Plain HTTPS rather than azure-storage-blob, so the SAS mode installs nothing.
    The endpoint suffix default matches the streamer's own.
    """
    suffix = os.environ.get("AZURE_STORAGE_ENDPOINT_SUFFIX", "core.windows.net")
    url = (
        f"https://{account}.blob.{suffix}/{urllib.parse.quote(container)}"
        f"?restype=container&comp=list&prefix={urllib.parse.quote(prefix)}"
        f"&{sas.lstrip('?')}"
    )
    with urllib.request.urlopen(url, timeout=60) as resp:
        body = resp.read()
    return [e.text or "" for e in ET.fromstring(body).iter("Name")]


def _probe_sas(uri: str) -> int:
    """Validate the SAS path: the token minted, and the container holds weights."""
    account = os.environ.get("AZURE_STORAGE_ACCOUNT_NAME")
    token = os.environ.get("AZURE_STORAGE_SAS_TOKEN")
    if not account or not token:
        return _fail(
            "fetch-sas exported no AZURE_STORAGE_ACCOUNT_NAME/AZURE_STORAGE_SAS_TOKEN; "
            "the SAS mint did not complete"
        )

    parts = urllib.parse.urlsplit(uri)
    container, prefix = parts.netloc, parts.path.lstrip("/")
    try:
        names = _list_blobs(account, container, prefix, token)
    except urllib.error.HTTPError as exc:
        # exc carries the SAS-bearing URL, so report only code and reason.
        return _fail(f"listing {_scrub(uri)} failed: HTTP {exc.code} {exc.reason}")
    except Exception as exc:
        return _fail(f"listing {_scrub(uri)} failed: {type(exc).__name__}")

    tensors = [n for n in names if n.endswith(".safetensors")]
    if not tensors:
        return _fail(
            f"no *.safetensors under {_scrub(uri)} ({len(names)} blob(s) listed) — the "
            "weights are missing or the streaming config points at the wrong container"
        )
    print(f"OK: minted SAS and listed {len(tensors)} safetensors file(s)")
    return 0


def _probe_stream(uri: str) -> int:
    """Validate the workload-identity path by reading one tensor."""
    from runai_model_streamer import SafetensorsStreamer, list_safetensors

    files = list_safetensors(uri)
    if not files:
        return _fail(
            f"no *.safetensors found under {_scrub(uri)} — the model weights are "
            "missing or the streaming configuration points at the wrong container"
        )
    print(f"found {len(files)} safetensors file(s)")

    target = sorted(files)[0]
    print(f"streaming {_scrub(target)} into CPU memory")

    with SafetensorsStreamer() as streamer:
        streamer.stream_file(target, device="cpu", is_distributed=False)
        # get_tensors() is annotated Iterator[torch.tensor] upstream but yields
        # (name, tensor) pairs. One tensor is enough to prove the path works.
        for name, tensor in streamer.get_tensors():
            print(f"OK: read tensor {name} with shape {tuple(tensor.shape)}")
            return 0

    return _fail(f"streamed {_scrub(target)} but read no tensors")


def main() -> int:
    timeout = int(os.environ.get("PROBE_TIMEOUT_SECONDS", DEFAULT_TIMEOUT_SECONDS))
    _start_watchdog(timeout)

    uri = os.environ.get("STREAM_MODEL_URI") or os.environ.get("PROBE_MODEL_URI")
    if not uri:
        return _fail(
            "no model URI: neither STREAM_MODEL_URI (from the SAS env file) "
            "nor PROBE_MODEL_URI is set"
        )

    mode = os.environ.get("PROBE_MODE", "unknown")
    print(f"probing model streaming (mode={mode}, uri={_scrub(uri)})")

    if mode == "sas":
        return _probe_sas(uri)
    return _probe_stream(uri)


if __name__ == "__main__":
    sys.exit(main())
