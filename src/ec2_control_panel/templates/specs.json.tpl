{
    "ImageId" : "{{ AMI_ID }}",
    "InstanceType": "{{ INSTANCE_TYPE }}",
    "KeyName" : "{{ PUB_KEY }}",
    "EbsOptimized": true,
    "Placement": {
        "AvailabilityZone": "{{ AVAILABILITY_ZONE }}"
    },
    "IamInstanceProfile": {
        "Arn": "{{ INSTANCE_ROLE }}"
    },
    "BlockDeviceMappings": [
        {
            "DeviceName": "/dev/sda1",
            "Ebs": {
                "DeleteOnTermination": true,
                "VolumeType": "gp3",
                "VolumeSize": {{ VOLUME_SIZE }}
            }
        }
    ],
    "NetworkInterfaces": [
        {
            "DeviceIndex": 0,
            "NetworkInterfaceId": "{{ ENI_ID }}"
        }
    ],
    "UserData" : "{{ USER_DATA }}"
}
