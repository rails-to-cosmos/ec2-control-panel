import os
import sys
from typing import Literal, Optional

import fire  # type: ignore

from ec2.data.efs import EFS
from ec2.data.geo import Geo
from ec2.data.instance import Spot, OnDemand, Instance
from ec2.data.network import ENI
from ec2.data.user_data import UserData
from ec2.data.volume import Volume
from ec2.data.vpc import VPC

RequestType = Literal["spot", "ondemand"]

NOT_FOUND = "Not found"

DEFAULT_AMI_ID = os.environ["EC2_AMI_ID"]
DEFAULT_AVAILABILITY_ZONE = os.environ["EC2_AVAILABILITY_ZONE"]
DEFAULT_INSTANCE_NAME = os.getenv("EC2_INSTANCE_NAME", os.getlogin())
DEFAULT_INSTANCE_ROLE = os.getenv("EC2_ROLE", "")
DEFAULT_INSTANCE_TYPE = os.getenv("EC2_INSTANCE_TYPE", "r5.large")
DEFAULT_PERSISTENT_NAME = os.getenv("EC2_PERSISTENT_NAME", os.getlogin())
DEFAULT_PUBLIC_KEY = os.getenv("EC2_PUBLIC_KEY", "")
DEFAULT_REGION = os.environ["EC2_REGION"]
DEFAULT_REQUEST_TYPE: RequestType = "spot"
DEFAULT_VOLUME_SIZE = int(os.getenv("EC2_VOLUME_SIZE") or 512)
DEFAULT_VPC_ID = os.environ["EC2_VPC_ID"]


class App:
    def status(self,
               persistent_name: str = DEFAULT_PERSISTENT_NAME,
               region: str = DEFAULT_REGION,
               availability_zone: str = DEFAULT_AVAILABILITY_ZONE,
               vpc_id: str = DEFAULT_VPC_ID) -> None:
        "Show current state for the ec2 instance."

        print(f"ID (Persistent name): {persistent_name}")
        vpc = VPC(id=vpc_id)
        print(f"VPC: {vpc}")

        geo = Geo(region=region, availability_zone=availability_zone, vpc=vpc)
        print(f"Region: {region}")
        print(f"Availability zone: {availability_zone}")

        volume: Optional[Volume] = Volume.get(name=persistent_name, geo=geo)
        eni: Optional[ENI] = ENI.get(name=persistent_name, geo=geo)

        if volume and eni and (instance := Instance.get(eni=eni, volume=volume)):
            print(f"Instance: {instance}")
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

    def start(self,
              persistent_name: str = DEFAULT_PERSISTENT_NAME,
              request_type: RequestType = DEFAULT_REQUEST_TYPE,
              instance_name: str = DEFAULT_INSTANCE_NAME,
              instance_type: str = DEFAULT_INSTANCE_TYPE,
              region: str = DEFAULT_REGION,
              availability_zone: str = DEFAULT_AVAILABILITY_ZONE,
              ami_id: str = DEFAULT_AMI_ID,
              pub_key: str = DEFAULT_PUBLIC_KEY,
              instance_role: str = DEFAULT_INSTANCE_ROLE,
              volume_size: int = DEFAULT_VOLUME_SIZE,
              vpc_id: str = DEFAULT_VPC_ID) -> None:
        "Start your lovely instance."

        print(f"Using persistent name to identify the persistent volume and network: {persistent_name}")
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
        volume: Volume
        volume_opt: Optional[Volume] = Volume.get(name=persistent_name, geo=geo)
        eni: ENI = ENI.get_or_create(name=persistent_name, geo=geo)

        if volume_opt and (running_instance := Instance.get(eni=eni, volume=volume_opt)):
            print(f"Instance is already running: {running_instance.id}")
            return

        if not volume_opt and input(f"Do you want to create volume {persistent_name} ({volume_size}Gb)? [y/n] ") == "y":
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
            )

            try:
                volume = temp_spot.persist_volume(persistent_name=persistent_name)
            finally:
                temp_spot.terminate()
        elif volume_opt:
            volume = volume_opt
        else:
            sys.exit(1)

        user_data = UserData.make_remount(volume=volume)

        instance: Instance

        print(f"Requesting {request_type} instance...")
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
            )

        instance.wait().should_not_fail()
        volume.wait_in_use()

        self.status(persistent_name=persistent_name,
                    region=region,
                    availability_zone=availability_zone)

    def stop(self,
             persistent_name: str = DEFAULT_PERSISTENT_NAME,
             region: str = DEFAULT_REGION,
             availability_zone: str = DEFAULT_AVAILABILITY_ZONE,
             vpc_id: str = DEFAULT_VPC_ID) -> None:
        "Stop running instance."

        vpc = VPC(id=vpc_id)
        geo = Geo(region=region, availability_zone=availability_zone, vpc=vpc)
        volume = Volume.get(name=persistent_name, geo=geo)

        if volume:
            print(f"Volume \"{persistent_name}\" found: {volume}")
        else:
            print(f"Volume \"{persistent_name}\" not found")
            sys.exit(1)

        eni = ENI.get_or_create(name=persistent_name, geo=geo)
        instance = Instance.get(eni=eni, volume=volume)

        if instance and instance.id:
            print(f"Instance found: {instance}")
            print(f"Waiting for instance {instance} to be available...")
            instance.wait()
            print(f"Shutting down instance {instance}...")
            instance.terminate()
        else:
            print(f"No instance {persistent_name} found: nothing to terminate")

    def restart(self,
                persistent_name: str = DEFAULT_PERSISTENT_NAME,
                request_type: RequestType = DEFAULT_REQUEST_TYPE,
                instance_name: str = DEFAULT_INSTANCE_NAME,
                instance_type: str = DEFAULT_INSTANCE_TYPE,
                region: str = DEFAULT_REGION,
                availability_zone: str = DEFAULT_AVAILABILITY_ZONE,
                ami_id: str = DEFAULT_AMI_ID,
                pub_key: str = DEFAULT_PUBLIC_KEY,
                instance_role: str = DEFAULT_INSTANCE_ROLE,
                vpc_id: str = DEFAULT_VPC_ID) -> None:
        "Restart existing instance. Apply another specification."

        self.stop(persistent_name=persistent_name,
                  region=region,
                  availability_zone=availability_zone,
                  vpc_id=vpc_id)

        self.start(persistent_name=persistent_name,
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
           persistent_name: str = DEFAULT_PERSISTENT_NAME,
           region: str = DEFAULT_REGION,
           availability_zone: str = DEFAULT_AVAILABILITY_ZONE,
           vpc_id: str = DEFAULT_VPC_ID) -> None:
        vpc = VPC(id=vpc_id)
        geo = Geo(region=region, availability_zone=availability_zone, vpc=vpc)
        volume = Volume.get(name=persistent_name, geo=geo)
        eni = ENI.get_or_create(name=persistent_name, geo=geo)

        if volume and eni and (instance := Instance.get(eni=eni, volume=volume)):
            print(instance.private_ip)

    def mount(self,
              volume_name: str,
              persistent_name: str = DEFAULT_PERSISTENT_NAME,
              region: str = DEFAULT_REGION,
              availability_zone: str = DEFAULT_AVAILABILITY_ZONE,
              vpc_id: str = DEFAULT_VPC_ID) -> None:
        vpc = VPC(id=vpc_id)
        geo = Geo(region=region, availability_zone=availability_zone, vpc=vpc)

        _efs: EFS | None = EFS.get(name=volume_name, geo=geo)
        if not _efs:
            if input(f"Volume {volume_name} not found in {geo}. Create one? (y/n) ") == "y":
                _efs = EFS.ensure(name=volume_name, geo=geo)
            else:
                print("Operation cancelled by user")
                return None
        efs: EFS = _efs

        volume = Volume.get(name=persistent_name, geo=geo)
        eni = ENI.get_or_create(name=persistent_name, geo=geo)

        if volume and eni and (instance := Instance.get(eni=eni, volume=volume)):
            instance.mount(efs).should_not_fail()


def main() -> None:
    return fire.Fire(App)


if __name__ == "__main__":
    main()
