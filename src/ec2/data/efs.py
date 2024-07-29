import time

from typing_extensions import Self
import attrs

from ec2.data.geo import Geo
from ec2.commands import run_command


@attrs.define(frozen=True, kw_only=True)
class EFS:
    id: str
    name: str
    geo: Geo

    @classmethod
    def get(cls, name: str, geo: Geo) -> Self | None:
        get_file_system = run_command("aws", "efs", "describe-file-systems",
                                      "--creation-token", name,
                                      "--region", geo.region,
                                      "--output", "json")

        match get_file_system.json():
            case {"FileSystems": [{"FileSystemId": file_system_id}]}:
                pass
            case _:
                return None

        return cls(id=file_system_id, name=name, geo=geo)

    @classmethod
    def create(cls, name: str, geo: Geo) -> Self:
        create_file_system = run_command("aws", "efs", "create-file-system",
                                         "--encrypted",
                                         "--creation-token", name,
                                         "--tags", f"Key=Name,Value={name}",
                                         "--region", geo.region,
                                         "--output", "json")

        create_file_system.should_not_fail()

        match create_file_system.json():
            case {"FileSystemId": file_system_id}:
                pass
            case _response:
                raise ValueError(f"Unable to parse FileSystemId from {_response}")

        lifecycle_state = "creating"
        while lifecycle_state == "creating":
            get_file_system = run_command("aws", "efs", "describe-file-systems",
                                          "--creation-token", name,
                                          "--region", geo.region,
                                          "--output", "json")

            match get_file_system.json():
                case {"FileSystems": [{"LifeCycleState": lifecycle_state}]}:
                    pass
                case _:
                    raise RuntimeError("Unable to retrieve file system's lifecycle state")

            time.sleep(5)

        put_lifecycle_configuration = run_command("aws", "efs", "put-lifecycle-configuration",
                                                  "--file-system-id", file_system_id,
                                                  "--lifecycle-policies", "TransitionToIA=AFTER_30_DAYS",
                                                  "--region", geo.region)

        put_lifecycle_configuration.should_not_fail()

        return cls(id=file_system_id, name=name, geo=geo)

    @classmethod
    def ensure(cls, name: str, geo: Geo) -> Self:
        return cls.get(name, geo) or cls.create(name, geo)
