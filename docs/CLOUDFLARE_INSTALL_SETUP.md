# Setting up https://install.iris-lakehouse.bymarreco.com with Cloudflare Workers

This makes these commands work:

```sh
curl -fsSL https://install.iris-lakehouse.bymarreco.com | bash            # stable
curl -fsSL https://install.iris-lakehouse.bymarreco.com/snapshot | bash   # rolling development build
curl -fsSL https://install.iris-lakehouse.bymarreco.com/uninstall.sh | bash
```

## Current accurate steps (as of the latest Cloudflare UI)

### 1. Create the Worker

From the Workers & Pages page:

1. Click the big blue **Create application** button.
2. You will see the "Create a Worker" screen with these options:
   - Continue with GitHub
   - Connect GitLab
   - **Start with Hello World!**
   - Select a template
   - Upload your static files

3. **Click "Start with Hello World!"** — this is the fastest way for a simple script like the installer.

4. Give the Worker a name, for example:
   - `iris-install`
   - or `install-iris-lakehouse`

5. Click **Create** / **Deploy**.

You will now be in the code editor with the default "Hello World" Worker.

### 2. Replace the code with the installer logic

Delete everything and paste this exact code:

```js
// Worker for https://install.iris-lakehouse.bymarreco.com
// Makes these work:
//   curl -fsSL https://install.iris-lakehouse.bymarreco.com | bash            (stable)
//   curl -fsSL https://install.iris-lakehouse.bymarreco.com/snapshot | bash   (development build)
//   curl -fsSL https://install.iris-lakehouse.bymarreco.com/uninstall.sh | bash

export default {
  async fetch(request) {
    const url = new URL(request.url);
    const path = url.pathname;

    // Always fetch the real script from the repo
    const base = 'https://raw.githubusercontent.com/MateusAMP2119/iris-lakehouse/HEAD';

    if (path === '/' || path === '/install.sh') {
      const target = `${base}/install.sh`;
      return Response.redirect(target, 302);
    }

    // Snapshot channel: same installer, version pinned to the rolling
    // "snapshot" prerelease. Served inline (not a redirect) so the pin
    // line can be prepended to the script body.
    if (path === '/snapshot') {
      const res = await fetch(`${base}/install.sh`);
      if (!res.ok) {
        return new Response('Upstream fetch of install.sh failed', { status: 502 });
      }
      const script = await res.text();
      const pinned = 'IRIS_VERSION="${IRIS_VERSION:-snapshot}"\n' + script;
      return new Response(pinned, {
        headers: { 'content-type': 'text/plain; charset=utf-8' }
      });
    }

    if (path === '/uninstall.sh') {
      const target = `${base}/uninstall.sh`;
      return Response.redirect(target, 302);
    }

    return new Response('Not found. Supported paths: /, /install.sh, /snapshot, /uninstall.sh', {
      status: 404,
      headers: { 'content-type': 'text/plain' }
    });
  }
};
```

Click **Save and deploy** (or the Deploy button at the top).

### 3. Test it immediately (using the free workers.dev URL)

After deployment, Cloudflare gives you a free URL like:

`https://iris-install.mateus-costa464.workers.dev`

Test it:

```sh
curl -I https://iris-install.mateus-costa464.workers.dev
curl -I https://iris-install.mateus-costa464.workers.dev/install.sh
```

You should get a 302 redirect to the GitHub raw install.sh.

You can even run the full installer against the workers.dev URL for testing:

```sh
curl -fsSL https://iris-install.mateus-costa464.workers.dev | bash
```

(Use `IRIS_DEST=<dir>` to install into a throwaway directory while testing.)

### 4. Attach your real domain

Once the Worker is working:

1. Go back to the Workers list.
2. Click on your Worker (`iris-install` or whatever you named it).
3. Go to the **Settings** tab (left sidebar or top navigation).
4. Look for the section called **Domains & Routes** (sometimes under **Triggers**).
5. Click **Add** → **Custom domain**.
6. Type exactly: `install.iris-lakehouse.bymarreco.com`
7. Confirm / Add.

Cloudflare will handle the DNS routing because `*.bymarreco.com` (or the root) is already in your account.

It can take 30–60 seconds for the custom domain to become active.

### 5. Final test

```sh
curl -fsSL https://install.iris-lakehouse.bymarreco.com | bash
```

This should now behave exactly like the long raw GitHub URL.

---

## Alternative: Serve content directly (no redirect)

If you want the short URL to stay in the user's terminal history (no 302), replace the redirect parts with this:

```js
if (path === '/' || path === '/install.sh') {
  const res = await fetch(`${base}/install.sh`);
  return new Response(res.body, {
    headers: { 'content-type': 'text/plain; charset=utf-8' }
  });
}
```

Same for uninstall.sh.

---

## Need help right now?

Reply with a screenshot of whatever screen you are on after clicking "Start with Hello World!", and I'll give you the next exact clicks.

The repo is already updated so that the docs and script headers recommend:

```sh
curl -fsSL https://install.iris-lakehouse.bymarreco.com | bash
```