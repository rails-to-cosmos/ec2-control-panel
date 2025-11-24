import os
import sys
from typing import Literal
from typing import Optional

import attrs
import fire  # type: ignore

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

RequestType = Literal["spot", "ondemand"]

NOT_FOUND = "Not found"

AMI_ID = os.environ["EC2_AMI_ID"]
AVAILABILITY_ZONE = os.environ["EC2_AVAILABILITY_ZONE"]
INSTANCE_ROLE = os.environ["EC2_ROLE"]
INSTANCE_TYPE = os.getenv("EC2_INSTANCE_TYPE", "r5.large")
PUBLIC_KEY = os.environ["EC2_PUBLIC_KEY"]
REGION = os.environ["EC2_REGION"]
BID_PRICE = str(os.getenv("EC2_SPOT_BID_PRICE", 1))

REQUEST_TYPE: RequestType

match os.getenv("EC2_REQUEST_TYPE"):
    case None:
        REQUEST_TYPE = "spot"
    case "spot":
        REQUEST_TYPE = "spot"
    case "ondemand":
        REQUEST_TYPE = "ondemand"
    case _request_type:
        raise ValueError(f"Unable to determine provided request type '{_request_type}': should be either 'spot' or 'ondemand'")


INSTANCE_VOLUME_SIZE = int(os.getenv("EC2_INSTANCE_VOLUME_SIZE", 30))
VOLUME_SIZE = int(os.getenv("EC2_VOLUME_SIZE") or 512)
VPC_ID = os.environ["EC2_VPC_ID"]
SECURITY_GROUP = os.environ["EC2_SECURITY_GROUP"]


@attrs.define
class Status:
    volume: Volume | None
    instance: Instance | None


@attrs.define(frozen=True, kw_only=True)
class App:
    aws_access_key_id: str = attrs.field(factory=lambda: os.environ["AWS_ACCESS_KEY_ID"])
    aws_secret_access_key: str = attrs.field(factory=lambda: os.environ["AWS_SECRET_ACCESS_KEY"])
    aws_region: str = attrs.field(factory=lambda: os.environ["AWS_REGION"])

    def status(self,
               session_id: str,
               region: str = REGION,
               availability_zone: str = AVAILABILITY_ZONE,
               vpc_id: str = VPC_ID,
               security_group_id: str = SECURITY_GROUP) -> Status:
        "Show current state for the ec2 instance."

        print(f"Session ID: {session_id}")
        vpc = VPC(id=vpc_id)
        print(f"VPC: {vpc}")

        geo = Geo(region=region, availability_zone=availability_zone, vpc=vpc)
        print(f"Region: {region}")
        print(f"Availability zone: {availability_zone}")

        security_group = SecurityGroup(id=security_group_id)

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
              request_type: RequestType = REQUEST_TYPE,
              instance_name: str | None = None,
              instance_type: str = INSTANCE_TYPE,
              region: str = REGION,
              availability_zone: str = AVAILABILITY_ZONE,
              ami_id: str = AMI_ID,
              pub_key: str = PUBLIC_KEY,
              instance_role: str = INSTANCE_ROLE,
              volume_size: int = VOLUME_SIZE,
              vpc_id: str = VPC_ID,
              security_group_id: str = SECURITY_GROUP) -> None:
        "Start your lovely instance."

        instance_name = instance_name or session_id
        print(f"Session ID: {session_id}")
        print(f"Instance name: {instance_name}")
        print(f"Instance type: {instance_type}")
        print(f"Instance role: {instance_role}")
        print(f"Volume size: {volume_size}Gb")
        print(f"Request type: {request_type}")
        print(f"Region: {region}")
        print(f"Availability zone: {availability_zone}")
        print(f"AMI ID: {ami_id}")
        print(f"Public key: {pub_key}")

        vpc = VPC(id=vpc_id)
        geo = Geo(region=region, availability_zone=availability_zone, vpc=vpc)
        persistent_volume: Volume
        volume_opt: Optional[Volume] = Volume.get(name=session_id, geo=geo)
        security_group = SecurityGroup(id=security_group_id)
        eni: ENI = ENI.get_or_create(name=session_id, geo=geo, security_group=security_group)

        if volume_opt and (running_instance := Instance.get(eni=eni, volume=volume_opt)):
            print(f"Instance is already running: {running_instance.id}")
            sys.exit(0)

        if not volume_opt:
            print("Create temp spot to persist volume")
            temp_spot = Spot.request(
                ami_id=ami_id,
                eni=eni,
                geo=geo,
                instance_role=instance_role,
                instance_type=instance_type,
                name=instance_name,
                pub_key=pub_key,
                user_data=UserData.make_reference(),
                volume_size=volume_size,
                bid_price=BID_PRICE,
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
            aws_access_key_id=self.aws_access_key_id,
            aws_secret_access_key=self.aws_secret_access_key,
            aws_region=self.aws_region,
        )

        print(f"Requesting {request_type} instance")
        if request_type.lower() == "ondemand":
            print(f"User requested {request_type.upper()} instance (resolved to ONDEMAND request type)")
            instance = OnDemand.request(
                name=instance_name,
                eni=eni,
                geo=geo,
                ami_id=ami_id,
                instance_type=instance_type,
                pub_key=pub_key,
                instance_role=instance_role,
                user_data=user_data,
                volume_size=INSTANCE_VOLUME_SIZE,
            )
        else:
            print(f"User requested {request_type.upper()} instance (resolved to SPOT request type)")
            instance = Spot.request(
                name=instance_name,
                ami_id=ami_id,
                instance_type=instance_type,
                instance_role=instance_role,
                pub_key=pub_key,
                eni=eni,
                geo=geo,
                user_data=user_data,
                volume_size=INSTANCE_VOLUME_SIZE,
                bid_price=BID_PRICE,
            )

        print("Waiting for instance to be available...")
        instance.wait().should_not_fail()

        if not instance.id:
            raise ValueError("Instance ID is None")

        self.status(session_id=session_id, region=region, availability_zone=availability_zone)

        print(f"Your instance \"{session_id}\" is ready to use")

    def stop(self,
             session_id: str,
             region: str = REGION,
             availability_zone: str = AVAILABILITY_ZONE,
             vpc_id: str = VPC_ID,
             security_group_id: str = SECURITY_GROUP) -> None:
        "Stop running instance."

        vpc = VPC(id=vpc_id)
        geo = Geo(region=region, availability_zone=availability_zone, vpc=vpc)
        volume = Volume.get(name=session_id, geo=geo)

        if volume:
            print(f"Volume \"{session_id}\" found: {volume}")
        else:
            print(f"Volume \"{session_id}\" not found")
            return

        security_group = SecurityGroup(id=security_group_id)
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
                request_type: RequestType = REQUEST_TYPE,
                instance_name: str | None = None,
                instance_type: str = INSTANCE_TYPE,
                region: str = REGION,
                availability_zone: str = AVAILABILITY_ZONE,
                ami_id: str = AMI_ID,
                pub_key: str = PUBLIC_KEY,
                instance_role: str = INSTANCE_ROLE,
                vpc_id: str = VPC_ID) -> None:
        "Restart existing instance. Apply another specification."

        instance_name = instance_name or session_id

        self.stop(session_id=session_id,
                  region=region,
                  availability_zone=availability_zone,
                  vpc_id=vpc_id)

        self.start(session_id=session_id,
                   region=region,
                   request_type=request_type,
                   instance_name=instance_name,
                   instance_type=instance_type,
                   availability_zone=availability_zone,
                   ami_id=ami_id,
                   pub_key=pub_key,
                   instance_role=instance_role,
                   vpc_id=vpc_id)

    def ip(self,
           session_id: str,
           region: str = REGION,
           availability_zone: str = AVAILABILITY_ZONE,
           vpc_id: str = VPC_ID,
           security_group_id: str = SECURITY_GROUP) -> None:

        vpc = VPC(id=vpc_id)
        geo = Geo(region=region, availability_zone=availability_zone, vpc=vpc)
        volume = Volume.get(name=session_id, geo=geo)
        security_group = SecurityGroup(id=security_group_id)
        eni = ENI.get_or_create(name=session_id, geo=geo, security_group=security_group)

        if volume and eni and (instance := Instance.get(eni=eni, volume=volume)):
            print(instance.private_ip)

    def mount(self,
              volume_name: str,
              session_id: str,
              region: str = REGION,
              availability_zone: str = AVAILABILITY_ZONE,
              vpc_id: str = VPC_ID,
              security_group_id: str = SECURITY_GROUP) -> None:

        vpc = VPC(id=vpc_id)
        geo = Geo(region=region, availability_zone=availability_zone, vpc=vpc)
        security_group = SecurityGroup(id=security_group_id)

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


def main() -> None:
    return fire.Fire(App)


if __name__ == "__main__":
    main()
