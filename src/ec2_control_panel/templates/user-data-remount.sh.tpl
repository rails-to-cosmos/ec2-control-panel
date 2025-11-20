#!/bin/sh

export TERM="linux"
rm /var/lib/apt/lists/* -vf
echo "apt-getting" >> /root/log.txt 2>&1

apt-get update >> /root/log.txt 2>&1
apt-get install -y jq  >> /root/log.txt 2>&1
apt-get install -y python3-pip python-setuptools >> /root/log.txt 2>&1
apt-get install -y git >> /root/log.txt 2>&1
apt-get install -y unzip >> /root/log.txt 2>&1
apt-get install -y coreutils >> /root/log.txt 2>&1

# curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip" >> /root/log.txt 2>&1
# unzip awscliv2.zip >> /root/log.txt 2>&1
# ./aws/install -u >> /root/log.txt 2>&1

# echo "Attaching volume {{ VOLUME_ID }} as /dev/sdf" >> /root/log.txt 2>&1

# aws ec2 attach-volume \
#     --volume-id "${VOLUME_ID}" \
#     --instance-id "${INSTANCE_ID}" \
#     --device /dev/sdf --region "${REGION}" || exit 1

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

NEWMNT=/permaroot
OLDMNT=old-root
e2label "${DEVICE}" /permaroot
tune2fs "${DEVICE}" -U `uuidgen`
mkdir "${NEWMNT}"

# #
# # point of no return...
# # modify /sbin/init on the ephemeral volume to chain-load from the persistent EBS volume, and then reboot
# #

# mv /sbin/init /sbin/init.backup

# cat >/sbin/init <<EOF11
# #!/bin/sh
# mount $DEVICE $NEWMNT
# [ ! -d $NEWMNT/$OLDMNT ] && mkdir -p $NEWMNT/$OLDMNT

# cd $NEWMNT
# pivot_root . ./$OLDMNT

# for dir in /dev /proc /sys /run; do
#     echo "Moving mounted file system ${OLDMNT}\${dir} to \$dir."
#     mount --move ./${OLDMNT}\${dir} \${dir}
# done
# exec chroot . /sbin/init
# EOF11

# chmod +x /sbin/init

# reboot afterwards
