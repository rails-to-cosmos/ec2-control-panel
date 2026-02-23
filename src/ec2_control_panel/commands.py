from subprocess import Popen, PIPE
from typing import Any
from typing_extensions import Self

import re
import json
import attrs

from ec2_control_panel.logger import logger


# Pattern to parse AWS CLI error messages like:
# "An error occurred (ErrorCode) when calling the OperationName operation (extra): Detail message"
_AWS_ERROR_RE = re.compile(
    r"An error occurred \((?P<code>[^)]+)\) "
    r"when calling the (?P<operation>\S+) operation"
    r"(?: \([^)]*\))?"
    r": (?P<message>.+)",
    re.DOTALL,
)


class AWSError(RuntimeError):
    """A structured AWS CLI error with parsed fields for clean display."""

    def __init__(self, stderr: str, cmd: str = ""):
        self.raw = stderr.strip()
        self.cmd = cmd
        m = _AWS_ERROR_RE.search(self.raw)
        if m:
            self.code = m.group("code")
            self.operation = m.group("operation")
            self.detail = m.group("message").strip()
        else:
            self.code = None
            self.operation = None
            self.detail = self.raw
        super().__init__(self.raw)

    def __str__(self) -> str:
        if self.code:
            lines = [
                f"AWS Error [{self.code}] in {self.operation}:",
                f"  {self.detail}",
            ]
        else:
            lines = [f"Command failed: {self.detail}"]
        if self.cmd:
            lines.append(f"  Command: {self.cmd}")
        return "\n".join(lines)


def is_valid_output(output: str) -> bool:
    return bool(output) and output.strip() != "None"


@attrs.define(frozen=True, kw_only=True)
class InvalidOutput:
    cmd: str
    data: str


@attrs.define(frozen=True, kw_only=True)
class ProcessOutput:
    value: str | InvalidOutput | AWSError

    def result(self) -> str:
        "Obtain result or raise errors in case of runtime errors or unmeaningful results."

        match self.value:
            case str() as result:
                return result
            case InvalidOutput() as output:
                raise ValueError(f"Invalid output received from command {output.cmd}: {output.data}")
            case AWSError() as exc:
                raise exc

    def json(self) -> dict:
        return json.loads(self.result())

    def should_not_fail(self) -> Self:
        match self.value:
            case str():
                return self  # tolerate ok result
            case InvalidOutput():
                return self  # tolerate invalid output
            case AWSError() as exc:
                raise exc  # do not tolerate failures

    def optional(self) -> str | Any:
        match self.value:
            case str() as result:
                return result
            case InvalidOutput():
                return None
            case AWSError() as exc:
                raise exc


def run_command(*cmd: str) -> ProcessOutput:
    log = logger.getChild("run_command")
    cmd_str = " ".join(cmd)

    log.debug(f"$ {cmd_str}")

    proc = Popen(cmd, stdout=PIPE, stderr=PIPE, text=True)
    stdout, stderr = proc.communicate()

    if proc.returncode == 0:
        value = stdout.strip()

        log.debug(f"> {value}")

        if is_valid_output(value):
            return ProcessOutput(value=value)
        return ProcessOutput(value=InvalidOutput(cmd=cmd_str, data=value))

    error = AWSError(stderr, cmd=cmd_str)
    log.error(str(error))
    return ProcessOutput(value=error)
