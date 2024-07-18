# ec2 helpers

## Installation

### Poetry

```bash
poetry add git+ssh://git@github.com:rails-to-cosmos/ec2-control-panel.git
```

## Usage

```bash
# Common workflow

ec2 status  # Shows current status of ec2 entities that are associated with you (volume, network and instance)

ec2 start  # Starts an instance with default parameters (spot, r5.xlarge, instance name: your username)
ec2 restart --instance-type=r5.2xlarge  # Restarts your instance, but allocates more resources to the newly started
ec2 stop  # Stops running instance

# Examples
ec2 start --request-type=spot --instance-type=r5.xlarge --instance-name=custom-instance-name
ec2 start --request-type=ondemand --instance-type=r5.2xlarge --instance-name=custom-instance-name
ec2 restart --instance_type=r5.large --instance-name=custom-instance-name

# Help is available
ec2 --help
ec2 start --help
ec2 restart --help
ec2 stop --help
ec2 status --help
```
