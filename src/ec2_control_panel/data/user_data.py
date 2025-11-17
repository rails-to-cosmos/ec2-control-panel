import re
import base64

import attrs
from jinja2 import Environment, FileSystemLoader
from typing_extensions import Self

from ec2_control_panel.package import get_package_root
from ec2_control_panel.data.volume import Volume


@attrs.define(kw_only=True, frozen=True)
class UserData:
    data: str

    @classmethod
    def make_reference(cls) -> Self:
        root = get_package_root()
        template_dir = root / "templates"
        env = Environment(loader=FileSystemLoader(template_dir))
        content = env.get_template(name="user-data-reference.sh.tpl").render()
        b64content = base64.b64encode(content.encode("utf-8")).decode("utf-8")
        clean_content = re.sub(r'[\n\r]', '', b64content)
        return cls(data=clean_content)

    @classmethod
    def render(cls) -> Self:
        root = get_package_root()
        template_dir = root / "templates"
        env = Environment(loader=FileSystemLoader(template_dir))

        content = env.get_template(name="user-data-remount.sh.tpl").render()
        b64content = base64.b64encode(content.encode("utf-8")).decode("utf-8")
        clean_content = re.sub(r'[\n\r]', '', b64content)
        return cls(data=clean_content)
