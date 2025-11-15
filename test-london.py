from ec2_control_panel.__main__ import App

app = App()

kwargs = {
    "session_id": "apps",
    "request_type": "ondemand",
    "instance_type": "r5.xlarge",
    "region": "eu-west-2",
    "availability_zone": "eu-west-2a",
    "ami_id": "ami-025d96ef1907017c7",
    "pub_key": "ab-london",
    "instance_role": "arn:aws:iam::185298664982:instance-profile/ec2",
    "instance_volume_size": 32,
    "volume_size": 200,
    "vpc_id": "vpc-0f187f50dbff2e74b",
    "security_group_id": "sg-0d8826c703b8a4f86",
}

app.start(**kwargs)
