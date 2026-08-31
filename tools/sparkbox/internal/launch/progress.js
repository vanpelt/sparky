// The only script this door serves, and the reason page.go's
// Content-Security-Policy names a sha256 rather than 'none'.
//
// It exists because the create behind that button takes as long as fifteen
// seconds — a VM, a disk, a tag stamp and a secret push — and a plain form POST
// paints nothing at all while it runs. The page just sits there. What people do
// with a page that sits there is press the button again, which is the exact
// pressure createOrReuse's singleflight was written to absorb (see create.go);
// this is the other half of that fix, on the side that stops it happening.
//
// It is an ENHANCEMENT and never a dependency. Nothing here calls
// preventDefault on the first submit, so with JavaScript off, or blocked, or
// broken, the form posts exactly as it always did and the only thing lost is
// the spinner. The handler re-validates every value from the path, the query
// and the session regardless.
(function () {
  var form = document.querySelector("form[data-busy]");
  if (!form) {
    return;
  }
  var button = form.querySelector("button");
  var label = button ? button.textContent : "";
  var sent = false;

  form.addEventListener("submit", function (event) {
    if (sent) {
      // A second press cannot make a second sandbox — the server collapses
      // concurrent presses onto one create and re-resolves inside the flight —
      // but it can restart a request that is already in flight and make the
      // wait longer than it needed to be.
      event.preventDefault();
      return;
    }
    sent = true;
    document.body.classList.add("creating");
    if (button) {
      // Deliberately not button.disabled. A disabled control is dropped from
      // the submission, and browsers disagree about whether disabling one
      // inside its own submit handler cancels the navigation; aria-disabled
      // plus pointer-events says the same thing to a person and to a screen
      // reader without touching the POST that is already leaving.
      button.setAttribute("aria-disabled", "true");
      button.textContent = "Creating…";
    }
  });

  // A page restored from the back/forward cache comes back frozen exactly as it
  // was left — spinner spinning, button inert — for a create that finished
  // minutes ago. Cache-Control: no-store keeps this page out of that cache in
  // Chrome and Firefox already; this is for the browsers where it does not.
  window.addEventListener("pageshow", function (event) {
    if (!event.persisted) {
      return;
    }
    sent = false;
    document.body.classList.remove("creating");
    if (button) {
      button.removeAttribute("aria-disabled");
      button.textContent = label;
    }
  });
})();
