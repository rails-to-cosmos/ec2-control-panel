#!/bin/sh

# If the persistent volume device already exists, this is a re-run after reboot — skip
if lsblk /dev/nvme1n1 2>/dev/null || lsblk /dev/xvdf 2>/dev/null; then
    echo "Volume already attached, skipping" >> /root/log.txt
    exit 0
fi

export TERM="linux"
rm /var/lib/apt/lists/* -vf
echo "apt-getting" >> /root/log.txt 2>&1

apt-get update >> /root/log.txt 2>&1
apt-get install -y jq  >> /root/log.txt 2>&1
apt-get install -y python3-pip python-setuptools >> /root/log.txt 2>&1
apt-get install -y git >> /root/log.txt 2>&1
apt-get install -y unzip >> /root/log.txt 2>&1
apt-get install -y coreutils >> /root/log.txt 2>&1

curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip" >> /root/log.txt 2>&1
unzip awscliv2.zip >> /root/log.txt 2>&1
./aws/install -u >> /root/log.txt 2>&1

TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
    http://169.254.169.254/latest/meta-data/instance-id)
VOLUME_ID="{{ VOLUME_ID }}"

export INSTANCE_ID
export VOLUME_ID

echo "Attaching volume {{ VOLUME_ID }} as /dev/sdf" >> /root/log.txt 2>&1

export AWS_REGION="{{ AWS_REGION }}"

ATTACHED=0
for i in 1 2 3 4 5; do
    if aws ec2 attach-volume --volume-id "${VOLUME_ID}" \
        --instance-id "${INSTANCE_ID}" --device /dev/sdf \
        --region "${AWS_REGION}" >> /root/log.txt 2>&1; then
        ATTACHED=1
        break
    fi
    echo "Attach attempt $i failed, retrying in 15s..." >> /root/log.txt
    sleep 15
done
[ "$ATTACHED" -eq 0 ] && { echo "Failed to attach volume after 5 attempts" >> /root/log.txt; exit 1; }

while true; do
    echo "Waiting for device to attach..."
    if lsblk /dev/nvme1n1; then
        BLKDEVICE=/dev/nvme1n1
        DEVICE=/dev/nvme1n1p1
        break
    fi
    if lsblk /dev/xvdf; then
        BLKDEVICE=/dev/xvdf
        DEVICE=/dev/xvdf1
        break
    fi
    sleep 5
done

# Ready up for the swap
NEWMNT=/permaroot
OLDMNT=old-root
e2label "${DEVICE}" permaroot
tune2fs "${DEVICE}" -U `uuidgen`
mkdir "${NEWMNT}"

#
# point of no return...
# modify /sbin/init on the ephemeral volume to chain-load from the persistent EBS volume, and then reboot.
#

mv /sbin/init /sbin/init.backup

cat >/sbin/init <<EOF11
#!/bin/sh
mount $DEVICE $NEWMNT
[ ! -d $NEWMNT/$OLDMNT ] && mkdir -p $NEWMNT/$OLDMNT

cd $NEWMNT
pivot_root . ./$OLDMNT

for dir in /dev /proc /sys /run; do
    echo "Moving mounted file system ${OLDMNT}\${dir} to \$dir."
    mount --move ./${OLDMNT}\${dir} \${dir}
done
exec chroot . /sbin/init
EOF11
chmod +x /sbin/init

shutdown -r now
