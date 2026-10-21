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

"""Response lifecycle only; buffering and safe release belong to KAITO core."""

from guardrail_core.streaming_window import (
    StreamingBufferWindow,
    WindowEmitResult,
    WindowScanner,
)


class StreamingResponseWindow:
    """Own one shared window, with terminal finish, abort, block, and failure."""

    def __init__(self, scanner: WindowScanner, *, holdback_len: int) -> None:
        self._window = StreamingBufferWindow(scanner, holdback_len=holdback_len)
        self._closed = False

    @property
    def closed(self) -> bool:
        return self._closed

    @property
    def blocked(self) -> bool:
        return self._window.blocked

    @property
    def redacted(self) -> bool:
        return self._window.redacted

    def feed(self, text: str) -> WindowEmitResult:
        self._require_open()
        try:
            result = self._window.feed(text)
        except BaseException:
            # Cancellation must be terminal too; preserve the original failure.
            self.abort()
            raise
        if result.blocked:
            self.abort()
        return result

    def finish(self) -> WindowEmitResult:
        """Flush once, only after successful completion of the upstream stream."""
        self._require_open()
        self._closed = True
        return self._window.flush()

    def abort(self) -> None:
        """Terminate without scanning or releasing the retained tail."""
        self._closed = True

    def _require_open(self) -> None:
        if self._closed:
            raise RuntimeError("Streaming response window is closed.")
