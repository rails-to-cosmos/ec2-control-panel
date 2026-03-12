import configparser
import os

import pytest

from ec2_control_panel.config import Config, InstanceConfig


SAMPLE_INI = """\
[DEFAULT]
region = ap-northeast-1
availability_zone = ap-northeast-1d
vpc_id = vpc-042628b8054e095ef
security_group = sg-0004eeb822745ac47
ami_id = ami-0422aaf1bafed17b3
instance_type = r5.xlarge
instance_role = arn:aws:iam::185298664982:instance-profile/ec2
public_key = aws_ab
request_type = spot
bid_price = 1
volume_size = 512
instance_volume_size = 30

[alice]

[bob]
availability_zone = ap-northeast-1b
instance_type = g4dn.xlarge
"""


@pytest.fixture
def conf_file(tmp_path):
    p = tmp_path / "instances.conf"
    p.write_text(SAMPLE_INI)
    return p


@pytest.fixture
def cfg(conf_file):
    return Config.load(str(conf_file))


# --- Config.load ---

class TestLoad:
    def test_load_from_explicit_path(self, conf_file):
        cfg = Config.load(str(conf_file))
        assert isinstance(cfg, Config)

    def test_load_from_env_var(self, conf_file, monkeypatch):
        monkeypatch.setenv("EC2_CONFIG", str(conf_file))
        cfg = Config.load()
        assert cfg.list_instances() == ["alice", "bob"]

    def test_load_missing_file_raises(self, tmp_path):
        with pytest.raises(FileNotFoundError, match="Config file not found"):
            Config.load(str(tmp_path / "nonexistent.conf"))

    def test_load_search_paths_all_missing(self, tmp_path, monkeypatch):
        monkeypatch.delenv("EC2_CONFIG", raising=False)
        monkeypatch.chdir(tmp_path)  # no ./instances.conf here
        with pytest.raises(FileNotFoundError, match="searched"):
            Config.load()

    def test_load_finds_cwd_fallback(self, tmp_path, monkeypatch):
        monkeypatch.delenv("EC2_CONFIG", raising=False)
        monkeypatch.chdir(tmp_path)
        (tmp_path / "instances.conf").write_text(SAMPLE_INI)
        cfg = Config.load()
        assert cfg.list_instances() == ["alice", "bob"]


# --- Config.list_instances ---

class TestListInstances:
    def test_returns_section_names(self, cfg):
        assert cfg.list_instances() == ["alice", "bob"]

    def test_empty_config_returns_empty(self, tmp_path):
        p = tmp_path / "empty.conf"
        p.write_text("[DEFAULT]\nregion = x\n")
        cfg = Config.load(str(p))
        assert cfg.list_instances() == []


# --- Config.resolve ---

class TestResolve:
    def test_inherits_defaults(self, cfg):
        ic = cfg.resolve("alice")
        assert isinstance(ic, InstanceConfig)
        assert ic.session_id == "alice"
        assert ic.region == "ap-northeast-1"
        assert ic.availability_zone == "ap-northeast-1d"
        assert ic.vpc_id == "vpc-042628b8054e095ef"
        assert ic.security_group == "sg-0004eeb822745ac47"
        assert ic.ami_id == "ami-0422aaf1bafed17b3"
        assert ic.instance_type == "r5.xlarge"
        assert ic.instance_role == "arn:aws:iam::185298664982:instance-profile/ec2"
        assert ic.public_key == "aws_ab"
        assert ic.request_type == "spot"
        assert ic.bid_price == "1"
        assert ic.volume_size == 512
        assert ic.instance_volume_size == 30

    def test_section_override(self, cfg):
        ic = cfg.resolve("bob")
        assert ic.availability_zone == "ap-northeast-1b"
        assert ic.instance_type == "g4dn.xlarge"
        # non-overridden fields still inherit
        assert ic.region == "ap-northeast-1"
        assert ic.public_key == "aws_ab"

    def test_unknown_section_raises_key_error(self, cfg):
        with pytest.raises(KeyError):
            cfg.resolve("unknown-user")

    def test_volume_size_is_int(self, cfg):
        ic = cfg.resolve("alice")
        assert isinstance(ic.volume_size, int)
        assert isinstance(ic.instance_volume_size, int)

    def test_frozen_dataclass(self, cfg):
        ic = cfg.resolve("alice")
        with pytest.raises(AttributeError):
            ic.region = "us-east-1"


# --- Config.create ---

class TestCreate:
    def test_create_adds_section(self, cfg, conf_file):
        cfg.create("charlie")
        reloaded = Config.load(str(conf_file))
        assert "charlie" in reloaded.list_instances()
        ic = reloaded.resolve("charlie")
        # inherits defaults
        assert ic.region == "ap-northeast-1"

    def test_create_with_overrides(self, cfg, conf_file):
        cfg.create("dave", instance_type="p3.2xlarge", volume_size="1024")
        reloaded = Config.load(str(conf_file))
        ic = reloaded.resolve("dave")
        assert ic.instance_type == "p3.2xlarge"
        assert ic.volume_size == 1024
        # non-overridden fields still inherit
        assert ic.region == "ap-northeast-1"

    def test_create_duplicate_raises(self, cfg):
        with pytest.raises(configparser.DuplicateSectionError):
            cfg.create("alice")

    def test_create_persists_to_disk(self, cfg, conf_file):
        cfg.create("eve")
        text = conf_file.read_text()
        assert "[eve]" in text

    def test_create_bootstrap_new_file(self, tmp_path):
        new_file = tmp_path / "new.conf"
        # Build a Config pointing at a non-existent file but with DEFAULT populated
        parser = configparser.ConfigParser()
        parser[configparser.DEFAULTSECT] = {"region": "us-west-2"}
        config = Config(parser, str(new_file))
        config.create("frank", instance_type="t3.micro")
        assert new_file.exists()
        reloaded_parser = configparser.ConfigParser()
        reloaded_parser.read(str(new_file))
        assert "frank" in reloaded_parser.sections()
        assert reloaded_parser["frank"]["instance_type"] == "t3.micro"


# --- InstanceConfig ---

class TestInstanceConfig:
    def test_all_fields_present(self, cfg):
        ic = cfg.resolve("alice")
        fields = {f.name for f in ic.__dataclass_fields__.values()}
        expected = {
            "session_id", "region", "availability_zone", "vpc_id",
            "security_group", "ami_id", "instance_type", "instance_role",
            "public_key", "request_type", "bid_price", "volume_size",
            "instance_volume_size",
        }
        assert fields == expected

    def test_equality(self, cfg):
        a = cfg.resolve("alice")
        b = cfg.resolve("alice")
        assert a == b

    def test_different_sessions_not_equal(self, cfg):
        a = cfg.resolve("alice")
        b = cfg.resolve("bob")
        assert a != b
