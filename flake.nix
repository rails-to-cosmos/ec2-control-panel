{
  description = "Flake with environment variables and installed packages";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs supportedSystems f;
    in {
      devShells = forAllSystems (system: let
        pkgs = import nixpkgs { inherit system; };
      in {
        default = pkgs.mkShell {
          packages = with pkgs; [
            fzf
            fd
            jq
            nodePackages.bash-language-server
          ];

          shellHook = ''
            export DEVENV_ROOT=$(git rev-parse --show-toplevel 2>/dev/null || echo "$PWD")
            export FZF_DEFAULT_COMMAND="fd --type f --strip-cwd-prefix"
            export PYTHON_VERSION="3.10"
            export PROJECT_NAME=$(basename "$DEVENV_ROOT")-$PYTHON_VERSION
            export PYENV_PATH=.pyenv/$PROJECT_NAME

            eval "$(pyenv init --path)"
            eval "$(pyenv init -)"
            eval "$(pyenv virtualenv-init -)"

            [ -d "$PYENV_PATH" ] && source "$PYENV_PATH/bin/activate" || echo "Virtualenv doesn't exist, please wake"

            function wake() {
                set -e

                pyenv install -s $PYTHON_VERSION
                pyenv local $PYTHON_VERSION

                mkdir -p $PYENV_PATH
                python -m venv virtualenv $PYENV_PATH

                source "$PYENV_PATH/bin/activate"

                pip install --upgrade pip
                pip install poetry

                if [ -f "pyproject.toml" ]; then
                    echo "pyproject.toml found. Running poetry install..."
                    poetry install
                else
                    echo "pyproject.toml not found. Running poetry init..."
                    poetry init
                fi
            }

            export -f wake
          '';
        };
      });
    };
}
