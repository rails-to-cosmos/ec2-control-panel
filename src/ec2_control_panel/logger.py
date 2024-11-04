import sys
import logging

logger = logging.getLogger("ec2")
logger.setLevel(logging.INFO)
handler = logging.StreamHandler(sys.stdout)
handler.setLevel(logging.INFO)
formatter = logging.Formatter('%(asctime)s %(levelname)-8s %(name)s:%(lineno)-3s %(message)s')
handler.setFormatter(formatter)
logger.addHandler(handler)
