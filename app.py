import marimo

__generated_with = "0.17.8"
app = marimo.App(width="medium")


@app.cell
def _():
    import os

    AWS_ACCESS_KEY_ID = os.environ["AWS_ACCESS_KEY_ID"]
    AWS_SECRET_ACCESS_KEY = os.environ["AWS_SECRET_ACCESS_KEY"]
    AWS_REGION = os.environ["AWS_REGION"]
    return AWS_ACCESS_KEY_ID, AWS_REGION, AWS_SECRET_ACCESS_KEY, os


@app.cell
def _():
    import marimo as mo
    import functools

    import boto3
    from botocore.exceptions import ClientError
    import requests

    from ec2_control_panel.__main__ import App, AVAILABILITY_ZONE
    return AVAILABILITY_ZONE, App, ClientError, boto3, functools, mo


@app.cell
def _(os):
    ec2_instances = os.getenv("EC2_INSTANCES", "default")
    ec2_instance_list = ec2_instances.split(", ")
    ec2_instance_list.sort()
    return (ec2_instance_list,)


@app.cell
def _(ec2_instance_list, mo):
    session_id = mo.ui.dropdown(label="Instance: ", options=ec2_instance_list, value=None, allow_select_none=True)
    session_id
    return (session_id,)


@app.cell
def _(mo, session_id):
    mo.stop(session_id is None)
    return


@app.cell
def _(App):
    app = App()
    return (app,)


@app.cell
def _(mo):
    refresh_button = mo.ui.refresh()
    mo.hstack([refresh_button], justify="end")
    return (refresh_button,)


@app.cell
def _(app, mo, refresh_button, session_id):
    status = None
    refresh_button.value

    with mo.status.spinner(subtitle="Getting instance status ..."):
        status = app.status(session_id=session_id.value)
    return (status,)


@app.cell
def _(mo, session_id, status):
    report = [
        {"Key": "InstanceName", "Value": session_id.value},
    ]

    if status.instance:
        for _key, _value in status.instance.system_info.items():
            report.append({"Key": _key, "Value": _value})
        report.append({"Key": "Private IP", "Value": status.instance.private_ip})
        report.append({"Key": "SSH", "Value": f"ssh ubuntu@{status.instance.private_ip}"})
    else:
        report.append({"Key": "Status", "Value": "Not Running"})

    mo.ui.table(report)
    return


@app.cell
def _(mo, status):
    mo.stop(status is None)

    mo.md("""**Manage Your Instance**""")
    return


@app.cell
def _(mo, status):
    _request_label = "Restart" if status.instance else "Start"
    start_button = mo.ui.run_button(label=_request_label, kind="success")
    stop_button = mo.ui.run_button(label="Stop", kind="danger", disabled=status.instance is None)
    return start_button, stop_button


@app.cell
def _(AWS_ACCESS_KEY_ID, AWS_REGION, AWS_SECRET_ACCESS_KEY, boto3):
    ec2 = boto3.client("ec2",
                       aws_access_key_id=AWS_ACCESS_KEY_ID,
                       aws_secret_access_key=AWS_SECRET_ACCESS_KEY,
                       region_name=AWS_REGION)
    return (ec2,)


@app.cell
def _(ClientError, ec2, functools):
    @functools.cache
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
def _(AVAILABILITY_ZONE, ec2, mo):
    instance_types = []

    paginator = ec2.get_paginator("describe_instance_type_offerings")
    for page in paginator.paginate(LocationType="availability-zone", Filters=[{"Name": "location", "Values": [AVAILABILITY_ZONE]}]):
        instance_types.extend([item["InstanceType"] for item in page["InstanceTypeOfferings"]])

    instance_types.sort()

    with mo.persistent_cache("ec2_instance_types"):
        instance_types
    return (instance_types,)


@app.cell
def _(instance_types, mo, status):
    instance_type_dropdown = mo.ui.dropdown(
        label="Instance Type:",
        value=status.instance.system_info["InstanceType"] if status.instance else "r5.xlarge",
        options=instance_types,
    )

    instance_type_dropdown
    return (instance_type_dropdown,)


@app.cell
def _(get_ec2_instance_info, instance_type_dropdown, mo):
    mo.plain_text(get_ec2_instance_info(instance_type_dropdown.value))
    return


@app.cell
def _(mo, status):
    mo.stop(status is None)

    request_type_dropdown = mo.ui.dropdown(
        label="Request Type:",
        value="ondemand",
        options=["ondemand", "spot"],
    )

    request_type_dropdown
    return (request_type_dropdown,)


@app.cell
def _(mo, start_button, stop_button):
    mo.hstack([start_button, stop_button], justify="start")
    return


@app.cell
def _(
    app,
    instance_type_dropdown,
    mo,
    request_type_dropdown,
    session_id,
    start_button,
    status,
):
    with mo.redirect_stderr(), mo.redirect_stdout():
        if start_button.value:
            with mo.status.spinner(subtitle="Processing your request ..."), mo.redirect_stdout():
                if status.instance and status.instance.system_info["InstanceType"] != instance_type_dropdown.value:
                    instance_type = status.instance.system_info["InstanceType"]
                    print(f"Changing instance type from {instance_type} to {instance_type_dropdown.value} ...")
                    app.restart(session_id=session_id.value,
                                instance_type=instance_type_dropdown.value,
                                request_type=request_type_dropdown.value)
                elif status.instance and status.instance.system_info["InstanceType"] == instance_type_dropdown.value:
                    print("Restarting the instance with no change to the resources ...")
                    app.restart(session_id=session_id.value,
                                instance_type=instance_type_dropdown.value,
                                request_type=request_type_dropdown.value)
                else:
                    print("Starting an instance ...")
                    app.start(session_id=session_id.value,
                              instance_type=instance_type_dropdown.value,
                              request_type=request_type_dropdown.value)
    return


@app.cell
def _(app, mo, session_id, status, stop_button):
    with mo.redirect_stderr(), mo.redirect_stdout():
        if stop_button.value:
            with mo.status.spinner(subtitle="Stopping your instance ..."), mo.redirect_stdout():
                if status.instance:
                    app.stop(session_id=session_id.value)
    return


if __name__ == "__main__":
    app.run()
