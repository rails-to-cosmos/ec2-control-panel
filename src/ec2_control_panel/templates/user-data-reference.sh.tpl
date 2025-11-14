#!/bin/sh

export TERM="linux"
rm /var/lib/apt/lists/* -vf

echo "apt-getting" >> /root/log.txt 2>&1
apt-get update >> /root/log.txt 2>&1
apt-get install -y jq  >> /root/log.txt 2>&1
apt-get install -y python-pip3 python-setuptools >> /root/log.txt 2>&1
apt-get install -y git >> /root/log.txt 2>&1

curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip" >> /root/log.txt 2>&1
unzip awscliv2.zip >> /root/log.txt 2>&1
./aws/install -u >> /root/log.txt 2>&1
