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
  // Every form on the page, not the first one. The reuse screen has two —
  // "open the one you have" and "create a new one" — and they post to
  // different URLs with opposite meanings, so a handler bound only to the
  // first would leave the second submitting into a page that paints nothing.
  var forms = document.querySelectorAll("form[data-busy]");
  if (!forms.length) {
    return;
  }
  var buttons = [];
  var labels = [];
  for (var i = 0; i < forms.length; i++) {
    var b = forms[i].querySelector("button");
    buttons.push(b);
    labels.push(b ? b.textContent : "");
  }
  // ONE flag across all of them, because the choice is exclusive: whichever
  // button was pressed, pressing the other now would abandon a create that is
  // already in flight and start a different one.
  var sent = false;

  function onSubmit(index) {
    return function (event) {
      if (sent) {
        // A second press cannot make a second sandbox — the server collapses
        // concurrent presses onto one create and re-resolves inside the flight
        // — but it can restart a request that is already in flight and make
        // the wait longer than it needed to be.
        event.preventDefault();
        return;
      }
      sent = true;
      document.body.classList.add("creating");
      for (var j = 0; j < buttons.length; j++) {
        if (!buttons[j]) {
          continue;
        }
        // Deliberately not button.disabled. A disabled control is dropped from
        // the submission, and browsers disagree about whether disabling one
        // inside its own submit handler cancels the navigation; aria-disabled
        // plus pointer-events says the same thing to a person and to a screen
        // reader without touching the POST that is already leaving.
        buttons[j].setAttribute("aria-disabled", "true");
        if (j === index) {
          buttons[j].textContent = "Creating…";
        }
      }
    };
  }

  for (var k = 0; k < forms.length; k++) {
    forms[k].addEventListener("submit", onSubmit(k));
  }

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
    for (var m = 0; m < buttons.length; m++) {
      if (!buttons[m]) {
        continue;
      }
      buttons[m].removeAttribute("aria-disabled");
      buttons[m].textContent = labels[m];
    }
  });
})();
