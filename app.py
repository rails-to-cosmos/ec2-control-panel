import marimo

__generated_with = "0.9.15"
app = marimo.App(width="medium")


@app.cell
def __():
    import marimo as mo
    import os
    import functools

    import boto3
    from botocore.exceptions import ClientError
    import requests

    from ec2_control_panel.__main__ import App
    return App, ClientError, boto3, functools, mo, os, requests


@app.cell
def __(mo):
    session_id = mo.ui.text(value="dmitry.akatov")
    return (session_id,)


@app.cell
def __(App):
    app = App()
    return (app,)


@app.cell
def __(mo):
    refresh_button = mo.ui.refresh(options=["1m", "5m", "10m"], default_interval="5m")
    mo.hstack([mo.md("**Current Status**"), refresh_button])
    return (refresh_button,)


@app.cell
def __(app, mo, refresh_button, session_id):
    status = None

    refresh_button
    with mo.status.spinner(subtitle="Loading data ..."):  # mo.redirect_stdout()
        status = app.status(session_id=session_id.value)
    return (status,)


@app.cell
def __(mo, session_id, status):
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
    return (report,)


@app.cell
def __(mo, status):
    mo.stop(status is None)

    mo.md("""**Manage Your Instance**""")
    return


@app.cell
def __(mo, status):
    _request_label = "Apply New Configuration" if status.instance else "Request New Instance"
    start_button = mo.ui.button(label=_request_label, value=0, on_click=lambda value: 1)
    stop_button = mo.ui.button(label="Stop", kind="danger", value=0, on_click=lambda value: 1)
    return start_button, stop_button


@app.cell
def __(ClientError, boto3, functools):
    @functools.cache
    def get_ec2_instance_info(instance_type: str):
        ec2 = boto3.client("ec2")

        try:
            response = ec2.describe_instance_types(InstanceTypes=[instance_type])
            instance_info = response['InstanceTypes'][0]

            vcpu_count = instance_info['VCpuInfo']['DefaultVCpus']
            memory_mib = instance_info['MemoryInfo']['SizeInMiB']
            memory_gb = memory_mib / 1024  # Convert MiB to GiB for easier readability

            result = (
                f"Instance Type: {instance_type}\n"
                f"CPU (vCPUs): {vcpu_count}\n"
                f"Memory: {memory_gb:.2f} GiB"
            )
            return result

        except ClientError as e:
            return f"Error retrieving info for {instance_type}: {e.response['Error']['Message']}"
    return (get_ec2_instance_info,)


@app.cell
def __(boto3, functools):
    @functools.cache
    def list_all_ec2_instance_types():
        ec2 = boto3.client('ec2')
        instance_types = []

        # Use a paginator to handle multiple API pages if needed
        paginator = ec2.get_paginator('describe_instance_types')
        for page in paginator.paginate():
            for instance in page['InstanceTypes']:
                instance_types.append(instance['InstanceType'])

        return sorted(instance_types)
    return (list_all_ec2_instance_types,)


@app.cell
def __(list_all_ec2_instance_types, mo, status):
    instance_type_dropdown = mo.ui.dropdown(
        label="Instance Type:", 
        value=status.instance.system_info["InstanceType"] if status.instance else "r5.xlarge",
        options=list_all_ec2_instance_types(),
    )

    instance_type_dropdown
    return (instance_type_dropdown,)


@app.cell
def __(get_ec2_instance_info, instance_type_dropdown):
    get_ec2_instance_info(instance_type_dropdown.value)
    return


@app.cell
def __(mo):
    request_type_dropdown = mo.ui.dropdown(
        label="Request Type:", 
        value="ondemand",
        options=["ondemand", "spot"],
    )

    request_type_dropdown
    return (request_type_dropdown,)


@app.cell
def __(mo, start_button, stop_button):
    mo.hstack([start_button, stop_button], justify="start")
    return


@app.cell
def __(
    app,
    instance_type_dropdown,
    mo,
    refresh_button,
    request_type_dropdown,
    session_id,
    start_button,
    status,
):
    mo.stop(start_button.value == 0)

    try:  
        with mo.status.spinner(subtitle="Processing your request ..."), mo.redirect_stdout():
            if status.instance and status.instance.system_info["InstanceType"] != instance_type_dropdown.value:
                instance_type = status.instance.system_info['InstanceType']
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
    finally:
        start_button.value = 0
        refresh_button.send_message({"action": "refresh"})
    return (instance_type,)


if __name__ == "__main__":
    app.run()