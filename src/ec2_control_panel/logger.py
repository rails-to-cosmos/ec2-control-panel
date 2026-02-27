import sys
import logging

logger = logging.getLogger("ec2")
logger.setLevel(logging.DEBUG)

_fmt = "%(asctime)s %(levelname)-8s %(name)s  %(message)s"
_datefmt = "%H:%M:%S"

# INFO and below go to stdout
_stdout = logging.StreamHandler(sys.stdout)
_stdout.setLevel(logging.DEBUG)
_stdout.addFilter(lambda r: r.levelno < logging.WARNING)
_stdout.setFormatter(logging.Formatter(_fmt, datefmt=_datefmt))

# WARNING and above go to stderr
_stderr = logging.StreamHandler(sys.stderr)
_stderr.setLevel(logging.WARNING)
_stderr.setFormatter(logging.Formatter(_fmt, datefmt=_datefmt))

logger.addHandler(_stdout)
logger.addHandler(_stderr)
