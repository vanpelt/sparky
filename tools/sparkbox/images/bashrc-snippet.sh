
# sparkbox: interactive shell polish (appended to /etc/bash.bashrc)
case $- in *i*)
  # Fall back to a known-good TERM when the client's terminal has no terminfo
  # entry in this guest. ghostty ships xterm-ghostty (baked below), but kitty and
  # friends do not — otherwise curses apps like top/htop silently fail to init.
  if ! infocmp "$TERM" >/dev/null 2>&1; then export TERM=xterm-256color; fi
  export STARSHIP_CONFIG=/etc/starship.toml
  if command -v starship >/dev/null 2>&1; then eval "$(starship init bash)"; fi
  if command -v direnv >/dev/null 2>&1; then eval "$(direnv hook bash)"; fi
  ;;
esac
