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

  scripts.wake.exec = ''
    set -e

    PROJECT_NAME=$(basename "$DEVENV_ROOT")

    pyenv install -s $PYTHON_VERSION
    pyenv local $PYTHON_VERSION
    pyenv version
    pyenv virtualenv $PROJECT_NAME || true
    echo "$PROJECT_NAME" > .python-version
    source $(pyenv root)/versions/$PROJECT_NAME/bin/activate

    pip install --upgrade pip
    pip install poetry

    if [ -f "pyproject.toml" ]; then
        echo "pyproject.toml found. Running poetry install..."
        poetry lock
        poetry install
    else
        echo "pyproject.toml not found. Running poetry init..."
        poetry init
    fi
  '';

  scripts.build.exec = ''
    poetry run pyinstaller $DEVENV_ROOT/src/ec2_control_panel/__main__.py
'';

  scripts.run-test.exec = ''
    poetry run mypy .
    poetry run pytest . $@
  '';

  scripts.ec2-connect.exec = ''
    ssh-keygen -R $(ec2 ip $@)
    ssh -o StrictHostKeyChecking=no ubuntu@$(ec2 ip $@)
  '';

  processes = {
    run-app.exec = "marimo run app.py --host=0.0.0.0 --port=2720";
  };

}
