{ pkgs, ... }:

{
  env.FZF_DEFAULT_COMMAND = "fd --type f --strip-cwd-prefix";
  env.PROJECT_NAME = "ec2-control-panel";
  env.PYTHON_VERSION = "3.10";

  packages = with pkgs; [
    fzf
    fd

    jq
    nodePackages.bash-language-server
  ];

  scripts.init.exec = ''
    pyenv install -s $PYTHON_VERSION
    pyenv local $PYTHON_VERSION
    pyenv version
    pyenv virtualenv $PROJECT_NAME
    echo "$PROJECT_NAME" > .python-version
  '';

  scripts.install.exec = ''
    pip install --upgrade pip
    pip install poetry
    poetry install
  '';

  scripts.run-test.exec = ''
    poetry run mypy .
    poetry run pytest . $@
  '';

}
