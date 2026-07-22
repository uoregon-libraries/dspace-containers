Extracting templates from the live site is painful. Here's some tips:

- Start with a browser plugin like SingleFile
  - Turn off the stupid infobar, there's no need for this
  - Visit the about page and save it
  - Copy it to `raw.html`
- Fix the HTML
  - For some reason there's no `<head>` tag; add one right after the opening
    `<html>` tag
- Reformat the HTML and extract CSS (automatic)
  - **Do not use HTML Tidy**: it breaks the output because it rewrites some
    attrs and elements, and it won't extract CSS, which is exceedingly helpful
    for modifying HTML in subsequent steps.
  - We created a custom tool for this, built to reformat HTML *without any
    rewrites*, and then extract all styles to external CSS so the HTML is easy
    to modify.
    - `make base-template` will read `tps-templates/raw.html` and generate
      `tps-templates/base.html`
    - (You'll need `make` and a Go compiler)
- Clean up the "base" HTML (manual)
  - Add `%HEAD%` directly under the `<head>` tag: this is where any extra head
    elements will be injected
  - Delete most meta tags, leave only what's critical (viewport info, encoding)
  - Delete the first `<button>` (the skip link; it's broken)
  - In `<title>` change `About` to `%TITLE%`, but preserve the rest if it makes
    sense (e.g., `Scholars' Bank :: %TITLE%`)
  - Remove the first `<nav>` block, then the `<ds-breadcrumbs>` block: these
    will make the page look "wrong", but it will be close enough, and both
    navigation sections will give you problems.
    - If you want to preserve these because you want a consistent look, you'll
      (a) have to make "Browse All" and "Log In" either show their dropdown or
      be disabled and (b) set up the breadcrumbs per template (challenge and
      fail). This is tougher than it should be.
  - Replace the entire contents of the page-level `<h1>` (the first is a
    site-level tag, so you don't want to grab the wrong one!) with
    `%PAGE-TITLE%`
  - Delete all `<p>` blocks in the main "about" section, replace with `<div>%BODY%</div>`
  - Your `<main>` should now look sort of like this:
    ```
    <main _ngcontent-dspace-angular-c2515629784 id=main-content class="my-cs ng-tns-c2515629784-0">
      <div _ngcontent-dspace-angular-c2515629784 class=ng-tns-c2515629784-0>
        <router-outlet _ngcontent-dspace-angular-c2515629784 class=ng-tns-c2515629784-0>
        </router-outlet>
        <ds-about class=ng-star-inserted data-used-theme=custom>
          <ds-about _nghost-dspace-angular-c1651741432 class=ng-star-inserted>
            <div _ngcontent-dspace-angular-c1651741432 class=about-container>
              <h1 _ngcontent-dspace-angular-c1651741432>%PAGE-TITLE%</h1>
              <div>
                %BODY%
              </div>
            </div>
          </ds-about>
        </ds-about>
      </div>
    </main>
    ```
- Commit `tps-templates/base.html` *and* `tps-templates/sb.css` to git (manual)
  - These are necessary for the assembler, and might need further tweaking, so
    you want them in git.
- Review the variable substitution files if desired; these are all found under
  `tps-templates`, and replace variables verbatim. If you make any changes,
  **commit them to git**!
  - Challenge page:
    - `challenge.head.html` replaces `%HEAD%`
      - This will likely only contain the JS snippet from the TPS challenge
        form, but could have other resource links if needed
    - `challenge.body.html` replaces `%BODY%`
      - This can be semi-complicated as it contains the TPS form and its
        supporting script, and any custom text necessary. We probably should
        always include a way to get help (e.g., an email address or a link to a
        contact form that is *not* protected by Cloudflare / Anubis) in case
        the challenge doesn't render properly
    - `challenge.title.txt` replaces `%TITLE%`
    - `challenge.page-title.txt` replaces `%PAGE-TITLE%`
    - The challenge page is critical to do right! See the vanilla [TPS
      challenge template][1] for reference.
      - The "head" HTML **must** include the Cloudflare `api.js` link
      - The "body" HTML **must** include the form *and* its corresponding script
        (the JS beacon and the on-submit behavior)
  - Failed page:
    - `failed.head.html` replaces `%HEAD%`
      - This will usually be empty for the failed form
    - `failed.body.html` replaces `%BODY%`
      - This should just be a message explaining that the user was detected as
        being a bot, and how to contact us
    - `failed.title.txt` replaces `%TITLE%`
    - `failed.page-title.txt` replaces `%PAGE-TITLE%`
- Assemble the final templates (automatic)
  - `make final-templates`
  - Fills in the "%"-surrounded variables
  - Recombines the CSS and HTML into a single file (for both challenge and
    failed templates) because TPS doesn't serve files directly
  - You'll end up with `tps-templates/challenge.go.html` and
    `tps-templates/failed.go.html`
- For prod, the templates will go into the SB-specific TPS template path
  - e.g., `/var/local/tps/scholarsbank.uoregon.edu/*.go.html`
  - I copy the templates to `tmp` since my normal user can't copy directly into the
    TPS location: `scp ./tps-templates/*.go.html $PROD:/tmp/`
- ???
- Profit!

[1]: <https://raw.githubusercontent.com/uoregon-libraries/turnstile-proxy-server/refs/heads/main/internal/templates/challenge.go.html>
