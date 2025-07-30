import marimo

__generated_with = "0.14.13"
app = marimo.App(width="medium")


@app.cell
def _():
    import marimo as mo
    import os
    import functools

    import boto3
    from botocore.exceptions import ClientError
    import requests

    from ec2_control_panel.__main__ import App
    return App, ClientError, boto3, mo


@app.cell
def _(App):
    app = App()
    return (app,)


@app.cell
def _():
    # REGIONS = sorted(["ap-northeast-1", "eu-west-2"])
    REGIONS = sorted(["eu-west-2"])
    return (REGIONS,)


@app.cell
def _():
    AZ_MAP = {
        "ap-northeast-1": ["ap-northeast-1d"],
        "eu-west-2": ["eu-west-2a"],
    }
    return (AZ_MAP,)


@app.cell
def _():
    VPC_MAP = {
        "ap-northeast-1": "vpc-042628b8054e095ef",
        "eu-west-2": "vpc-0f187f50dbff2e74b",
    }
    return (VPC_MAP,)


@app.cell
def _():
    SG_MAP = {
        "ap-northeast-1": "sg-0004eeb822745ac47",
        "eu-west-2": "sg-0d8826c703b8a4f86",
    }
    return (SG_MAP,)


@app.cell
def _():
    INSTANCE_MAP = {
        "ap-northeast-1": ["apps"],
        "eu-west-2": ["apps"],
    }
    return (INSTANCE_MAP,)


@app.cell
def _(REGIONS, mo):
    region_input = mo.ui.dropdown(
        label="Region: ", 
        options=REGIONS, 
        value=REGIONS[0], 
    )
    return (region_input,)


@app.cell
def _(AZ_MAP, INSTANCE_MAP, SG_MAP, VPC_MAP, region_input):
    region = region_input.value
    az_list = AZ_MAP.get(region)
    vpc_id = VPC_MAP.get(region)
    security_group_id = SG_MAP.get(region)
    instance_list = INSTANCE_MAP.get(region)
    return az_list, instance_list, region, security_group_id, vpc_id


@app.cell
def _(mo):
    refresh_button = mo.ui.refresh(options=["1m", "5m", "10m"], default_interval="5m")
    mo.hstack([refresh_button], justify="end")
    return (refresh_button,)


@app.cell
def _(az_list, mo, region_input):
    az_input = mo.ui.dropdown(label="Availability zone: ", options=az_list, value=az_list[0])

    mo.hstack([region_input, az_input], justify="start")
    return (az_input,)


@app.cell
def _(az_input):
    availability_zone = az_input.value
    return (availability_zone,)


@app.cell
def _(instance_list, mo):
    instance_input = mo.ui.dropdown(label="Instance: ", options=instance_list, value=None, allow_select_none=True)
    instance_input
    return (instance_input,)


@app.cell
def _(instance_input, mo):
    mo.stop(not instance_input.value)

    instance = instance_input.value
    return (instance,)


@app.cell
def _(boto3, region):
    ec2 = boto3.client("ec2", region_name=region)
    return (ec2,)


@app.cell
def _(
    app,
    availability_zone,
    instance,
    instance_input,
    mo,
    refresh_button,
    region,
    security_group_id,
    vpc_id,
):
    mo.stop(not instance_input.value)

    refresh_button.value

    with mo.status.spinner(subtitle="Loading data about your instance ..."):
        status = app.status(session_id=instance,
                            region=region,
                            availability_zone=availability_zone,
                            vpc_id=vpc_id,
                            security_group_id=security_group_id)
    return (status,)


@app.cell
def _(instance, mo, status):
    report = [
        {"Key": "InstanceName", "Value": instance},
    ]

    if status.instance:
        report.extend({"Key": _key, "Value": _value} for _key, _value in status.instance.system_info.items())
        report.append({"Key": "Private IP", "Value": status.instance.private_ip})
        report.append({"Key": "SSH", "Value": f"ssh ubuntu@{status.instance.private_ip}"})
    else:
        report.append({"Key": "Status", "Value": "Not Running"})

    mo.ui.table(report)
    return


@app.cell
def _(instance_input, mo):
    mo.stop(not instance_input.value)

    mo.md("""**Manage Your Instance**""")
    return


@app.cell
def _(mo, status):
    start_button = mo.ui.run_button(label="Restart" if status.instance else "Start", kind="success")
    stop_button = mo.ui.run_button(label="Stop", kind="danger", disabled=status.instance is None)
    return start_button, stop_button


@app.cell
def _(ClientError, ec2):
    def get_ec2_instance_info(instance_type: str):
        try:
            results = []
            response = ec2.describe_instance_types(InstanceTypes=[instance_type])
            for instance_info in response['InstanceTypes']:
                result = []
                vcpu_count = instance_info['VCpuInfo']['DefaultVCpus']
                memory_mib = instance_info['MemoryInfo']['SizeInMiB']
                memory_gb = memory_mib / 1024  # Convert MiB to GiB for easier readability

                result.append(f"Instance Type: {instance_type}")
                result.append(f"CPU (vCPUs): {vcpu_count}")
                result.append(f"Memory: {memory_gb:.2f} GiB")

                if "GpuInfo" in instance_info:
                    gpu_info = instance_info["GpuInfo"]
                    result.append(f"GPUs: {gpu_info['Gpus'][0]['Count']} x {gpu_info['Gpus'][0]['Name']}")
                    result.append(f"GPU Memory: {gpu_info['TotalGpuMemoryInMiB']} MiB")
                else:
                    result.append("No GPU information available.")

                results.append("\n".join(result))
            return "\n\n".join(results)

        except ClientError as e:
            return f"Error retrieving info for {instance_type}: {e.response['Error']['Message']}"
    return (get_ec2_instance_info,)


@app.cell
def _(availability_zone, ec2):
    instance_types = []

    paginator = ec2.get_paginator("describe_instance_type_offerings")

    pages = paginator.paginate(
        LocationType="availability-zone", 
        Filters=[{"Name": "location", "Values": [availability_zone]}],
    )

    for page in pages:
        instance_types.extend([item["InstanceType"] for item in page["InstanceTypeOfferings"]])

    instance_types.sort()
    return (instance_types,)


@app.cell
def _(instance_types, mo, status):
    instance_type_input = mo.ui.dropdown(
        label="Instance Type:",
        value=status.instance.system_info["InstanceType"] if status.instance else "r5.xlarge",
        options=instance_types,
        allow_select_none=False,
    )

    instance_type_input
    return (instance_type_input,)


@app.cell
def _(get_ec2_instance_info, instance_type_input, mo):
    mo.stop(not instance_type_input.value)

    instance_type = instance_type_input.value

    mo.md("```\n" + get_ec2_instance_info(instance_type).replace("\n", "\n") + "\n```")
    return (instance_type,)


@app.cell
def _(instance_type_input, mo):
    mo.stop(instance_type_input.value is None)

    request_type_input = mo.ui.dropdown(
        label="Request Type:",
        value="ondemand",
        options=["ondemand", "spot"],
    )

    request_type_input
    return (request_type_input,)


@app.cell
def _(mo, request_type_input):
    mo.stop(request_type_input.value is None)

    request_type = request_type_input.value
    return (request_type,)


@app.cell
def _(instance_type_input, mo, start_button, stop_button):
    mo.stop(instance_type_input.value is None)

    mo.hstack([start_button, stop_button], justify="start")
    return


@app.cell
def _(
    ami_id,
    app,
    availability_zone,
    instance,
    instance_role,
    instance_type,
    mo,
    pub_key,
    region,
    request_type,
    security_group_id,
    start_button,
    status,
    volume_size,
    vpc_id,
):
    with mo.redirect_stderr(), mo.redirect_stdout():
        if start_button.value:
            with mo.status.spinner(subtitle="Processing your request ..."), mo.redirect_stdout():
                if status.instance and status.instance.system_info["InstanceType"] != instance_type:
                    status_instance_type = status.instance.system_info["InstanceType"]
                    print(f"Changing '{instance}' instance type from {status_instance_type} to {instance_type} ...")
                    app.restart(session_id=instance,
                                instance_type=instance_type,
                                request_type=request_type)
                elif status.instance and status.instance.system_info["InstanceType"] == instance_type:
                    print(f"Restarting '{instance}' with no change to the resources ...")
                    app.restart(session_id=instance,
                                instance_type=instance_type,
                                request_type=request_type)
                else:
                    print("Starting '{instance}' ...")
                    app.start(session_id=instance,
                              request_type=request_type,
                              instance_type=instance_type,
                              region=region,
                              availability_zone=availability_zone,
                              ami_id=ami_id,
                              pub_key=pub_key,
                              instance_role=instance_role,
                              volume_size=volume_size,
                              vpc_id=vpc_id,
                              security_group_id=security_group_id)
    return


@app.cell
def _(app, instance, mo, status, stop_button):
    with mo.redirect_stderr(), mo.redirect_stdout():
        if stop_button.value:
            with mo.status.spinner(subtitle="Stopping your instance ..."), mo.redirect_stdout():
                if status.instance:
                    app.stop(session_id=instance)
    return


if __name__ == "__main__":
    app.run()
