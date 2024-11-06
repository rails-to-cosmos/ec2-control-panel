import importlib
from pathlib import Path

PACKAGE_NAME = "ec2_control_panel"


def get_package_root(package_name: str = PACKAGE_NAME):
    package = importlib.import_module(package_name)

    if not package.__file__:
        raise ValueError(f"Unable to find assotiated file with package {package}")

    package_dir = Path(package.__file__).parent
    return package_dir
