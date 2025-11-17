from subprocess import Popen, PIPE
from typing import Any
from typing_extensions import Self

import json
import attrs

from ec2_control_panel.logger import logger


def is_valid_output(output: str) -> bool:
    return bool(output) and output.strip() != "None"


@attrs.define(frozen=True, kw_only=True)
class InvalidOutput:
    cmd: str
    data: str


@attrs.define(frozen=True, kw_only=True)
class ProcessOutput:
    value: str | InvalidOutput | RuntimeError

    def result(self) -> str:
        "Obtain result or raise errors in case of runtime errors or unmeaningful results."

        match self.value:
            case str() as result:
                return result
            case InvalidOutput() as output:
                raise ValueError(f"Invalid output received from command {output.cmd}: {output.data}")
            case RuntimeError() as exc:
                raise exc

    def json(self) -> dict:
        return json.loads(self.result())

    def should_not_fail(self) -> Self:
        match self.value:
            case str():
                return self  # tolerate ok result
            case InvalidOutput():
                return self  # tolerate invalid output
            case RuntimeError() as exc:
                raise exc  # do not tolerate failures

    def optional(self) -> str | Any:
        match self.value:
            case str() as result:
                return result
            case InvalidOutput():
                return None
            case RuntimeError() as exc:
                raise exc


def run_command(*cmd: str) -> ProcessOutput:
    log = logger.getChild("run_command")

    proc = Popen(cmd, stdout=PIPE, stderr=PIPE, text=True)
    stdout, stderr = proc.communicate()

    if proc.returncode == 0:
        value = stdout.strip()

        log.debug(f"> {value}")

        if is_valid_output(value):
            return ProcessOutput(value=value)
        return ProcessOutput(value=InvalidOutput(cmd=' '.join(cmd), data=value))
    return ProcessOutput(value=RuntimeError(stderr))
