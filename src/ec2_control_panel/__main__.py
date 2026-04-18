import os
import sys
from typing import Optional

import attrs
import fire  # type: ignore

from ec2_control_panel.commands import AWSError
from ec2_control_panel.config import Config
from ec2_control_panel.data.efs import EFS
from ec2_control_panel.data.geo import Geo
from ec2_control_panel.data.instance import Instance
from ec2_control_panel.data.instance import OnDemand
from ec2_control_panel.data.instance import Spot
from ec2_control_panel.data.network import ENI
from ec2_control_panel.data.security_group import SecurityGroup
from ec2_control_panel.data.user_data import UserData
from ec2_control_panel.data.volume import Volume
from ec2_control_panel.data.vpc import VPC

NOT_FOUND = "Not found"

_config: Config | None = None


def get_config() -> Config:
    global _config
    if _config is None:
        _config = Config.load()
    return _config


@attrs.define
class Status:
    volume: Volume | None
    instance: Instance | None


@attrs.define(frozen=True, kw_only=True)
class App:
    aws_region: str = attrs.field(factory=lambda: os.environ["AWS_REGION"])

    def status(self, session_id: str) -> Status:
        "Show current state for the ec2 instance."

        cfg = get_config().resolve(session_id)

        print(f"Session ID: {session_id}")
        vpc = VPC(id=cfg.vpc_id)
        print(f"VPC: {vpc}")

        geo = Geo(region=cfg.region, availability_zone=cfg.availability_zone, vpc=vpc)
        print(f"Region: {cfg.region}")
        print(f"Availability zone: {cfg.availability_zone}")

        security_group = SecurityGroup(id=cfg.security_group)

        volume: Optional[Volume] = Volume.get(name=session_id, geo=geo)
        eni: Optional[ENI] = ENI.get(name=session_id,
                                     geo=geo,
                                     security_group=security_group)

        result = Status(volume=volume, instance=None)
        if volume and eni and (instance := Instance.get(eni=eni, volume=volume)):
            print(f"Instance: {instance}")
            result.instance = instance
            for key, value in instance.system_info.items():
                print(f"    {key}: {value}")
            print(f"    IP: {instance.private_ip}")
            print(f"    SSH: ssh ubuntu@{instance.private_ip}")
            print(f"    {instance.status}")
        else:
            print(f"Instance: {NOT_FOUND}")

        print(f"Geo: {geo}")
        print(f"Subnet: {geo.subnet_id}")
        print(f"Volume: {volume if volume else NOT_FOUND}")
        print(f"Network: {eni or NOT_FOUND}")

        return result

    def start(self,
              session_id: str,
              request_type: str | None = None,
              instance_type: str | None = None,
              instance_name: str | None = None) -> None:
        "Start your lovely instance."

        cfg = get_config().resolve(session_id)

        request_type = request_type or cfg.request_type
        instance_type = instance_type or cfg.instance_type
        instance_name = instance_name or session_id

        print(f"Session ID: {session_id}")
        print(f"Instance name: {instance_name}")
        print(f"Instance type: {instance_type}")
        print(f"Instance role: {cfg.instance_role}")
        print(f"Volume size: {cfg.volume_size}Gb")
        print(f"Request type: {request_type}")
        print(f"Region: {cfg.region}")
        print(f"Availability zone: {cfg.availability_zone}")
        print(f"AMI ID: {cfg.ami_id}")
        print(f"Public key: {cfg.public_key}")

        vpc = VPC(id=cfg.vpc_id)
        geo = Geo(region=cfg.region, availability_zone=cfg.availability_zone, vpc=vpc)
        persistent_volume: Volume
        volume_opt: Optional[Volume] = Volume.get(name=session_id, geo=geo)
        security_group = SecurityGroup(id=cfg.security_group)
        eni: ENI = ENI.get_or_create(name=session_id, geo=geo, security_group=security_group)

        if volume_opt and (running_instance := Instance.get(eni=eni, volume=volume_opt)):
            print(f"Instance is already running: {running_instance.id}")
            sys.exit(0)

        if volume_opt:
            volume_opt.force_detach_if_stale()

        if not volume_opt:
            print("Create temp spot to persist volume")
            temp_spot = Spot.request(
                ami_id=cfg.ami_id,
                eni=eni,
                geo=geo,
                instance_role=cfg.instance_role,
                instance_type=instance_type,
                name=instance_name,
                pub_key=cfg.public_key,
                user_data=UserData.make_reference(),
                volume_size=cfg.volume_size,
                bid_price=cfg.bid_price,
            )

            try:
                persistent_volume = temp_spot.persist_volume(persistent_name=session_id)
            finally:
                temp_spot.terminate()
        elif volume_opt:
            persistent_volume = volume_opt
        else:
            sys.exit(0)

        instance: Instance

        print("Prepare user data")

        user_data = UserData.chainload(
            volume=persistent_volume,
            aws_region=self.aws_region,
        )

        print(f"Requesting {request_type} instance")
        if request_type.lower() == "ondemand":
            print(f"User requested {request_type.upper()} instance (resolved to ONDEMAND request type)")
            instance = OnDemand.request(
                name=instance_name,
                eni=eni,
                geo=geo,
                ami_id=cfg.ami_id,
                instance_type=instance_type,
                pub_key=cfg.public_key,
                instance_role=cfg.instance_role,
                user_data=user_data,
                volume_size=cfg.instance_volume_size,
            )
        else:
            print(f"User requested {request_type.upper()} instance (resolved to SPOT request type)")
            instance = Spot.request(
                name=instance_name,
                ami_id=cfg.ami_id,
                instance_type=instance_type,
                instance_role=cfg.instance_role,
                pub_key=cfg.public_key,
                eni=eni,
                geo=geo,
                user_data=user_data,
                volume_size=cfg.instance_volume_size,
                bid_price=cfg.bid_price,
            )

        print("Waiting for instance to be available...")
        instance.wait().should_not_fail()

        if not instance.id:
            raise ValueError("Instance ID is None")

        self.status(session_id=session_id)

        print(f"Your instance \"{session_id}\" is ready to use")

    def stop(self, session_id: str) -> None:
        "Stop running instance."

        cfg = get_config().resolve(session_id)

        vpc = VPC(id=cfg.vpc_id)
        geo = Geo(region=cfg.region, availability_zone=cfg.availability_zone, vpc=vpc)
        volume = Volume.get(name=session_id, geo=geo)

        if volume:
            print(f"Volume \"{session_id}\" found: {volume}")
        else:
            print(f"Volume \"{session_id}\" not found")
            return

        security_group = SecurityGroup(id=cfg.security_group)
        eni = ENI.get_or_create(name=session_id, geo=geo, security_group=security_group)
        instance = Instance.get(eni=eni, volume=volume)

        if instance and instance.id:
            print(f"Instance found: {instance}")
            print(f"Waiting for instance {instance} to be available...")
            instance.wait()
            print(f"Shutting down instance {instance}...")
            instance.terminate()
        else:
            print(f"No instance {session_id} found: nothing to terminate")

    def restart(self,
                session_id: str,
                request_type: str | None = None,
                instance_type: str | None = None,
                instance_name: str | None = None) -> None:
        "Restart existing instance. Apply another specification."

        instance_name = instance_name or session_id

        self.stop(session_id=session_id)

        self.start(session_id=session_id,
                   request_type=request_type,
                   instance_name=instance_name,
                   instance_type=instance_type)

    def ip(self, session_id: str) -> None:
        cfg = get_config().resolve(session_id)

        vpc = VPC(id=cfg.vpc_id)
        geo = Geo(region=cfg.region, availability_zone=cfg.availability_zone, vpc=vpc)
        volume = Volume.get(name=session_id, geo=geo)
        security_group = SecurityGroup(id=cfg.security_group)
        eni = ENI.get_or_create(name=session_id, geo=geo, security_group=security_group)

        if volume and eni and (instance := Instance.get(eni=eni, volume=volume)):
            print(instance.private_ip)

    def mount(self,
              volume_name: str,
              session_id: str) -> None:

        cfg = get_config().resolve(session_id)

        vpc = VPC(id=cfg.vpc_id)
        geo = Geo(region=cfg.region, availability_zone=cfg.availability_zone, vpc=vpc)
        security_group = SecurityGroup(id=cfg.security_group)

        _efs: EFS | None = EFS.get(name=volume_name, geo=geo)
        if not _efs:
            if input(f"Volume {volume_name} not found in {geo}. Create one? (y/n) ") == "y":
                _efs = EFS.ensure(name=volume_name, geo=geo)
            else:
                print("Operation cancelled by user")
                return None
        efs: EFS = _efs

        volume = Volume.get(name=session_id, geo=geo)
        eni = ENI.get_or_create(name=session_id, geo=geo, security_group=security_group)

        if volume and eni and (instance := Instance.get(eni=eni, volume=volume)):
            instance.mount(efs).should_not_fail()

    def create(self, session_id: str, **overrides: str) -> None:
        "Register a new instance definition in the config file."
        config = get_config()
        config.create(session_id, **overrides)
        print(f"Instance '{session_id}' added to config.")


def main() -> None:
    try:
        return fire.Fire(App)
    except AWSError as exc:
        print(f"\n{'=' * 60}", file=sys.stderr)
        print(f"  {exc}", file=sys.stderr)
        print(f"{'=' * 60}", file=sys.stderr)
        sys.exit(1)
    except (RuntimeError, ValueError) as exc:
        msg = str(exc).strip()
        if msg:
            print(f"\nError: {msg}", file=sys.stderr)
        else:
            print(f"\nUnexpected error ({type(exc).__name__})", file=sys.stderr)
        sys.exit(1)
    except FileNotFoundError as exc:
        print(f"\n{exc}", file=sys.stderr)
        sys.exit(1)
    except KeyError as exc:
        print(f"\nMissing required configuration: {exc}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
