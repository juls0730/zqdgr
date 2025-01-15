# ZQDGR

ZQDGR is Zoe's Quick and Dirty Golang Runner. This is a simple tool that lets you run a go project in a similar way to how you would use npm. ZQDGR lets you watch files and rebuild your project as you make changes. ZQDGR also includes an optional websocket server that will notify listeners that a rebuild has occurred, this is very useful for live reloading when doing web development with Go.

## Install

```bash
go install github.com/juls0730/zqdgr@latest
```

## Usage

Full usage
```Bash
zqdgr [options] <command>
```

The list of commands is
- `init`

  generates a zqdgr.config.json file in the current directory

- `new [project name] [github repo]`
  
  Creates a new golang project with zqdgr and can optionally run scripts from a github repo

- `watch <script>`
  
  runs the script in "watch mode", when files that follow the pattern in zqdgr.config.json change, the script restarts
- `<script>`
  
  runs the script


ZQDGR has the following list of options
- `-no-ws`
  
  disables the web socket server running at 2067

Example usage:
```bash
zqdgr init
zqdgr watch dev
```

### ZQDGR websocket
ZQDGR comes with a websocket to notify listeners that the application has updates, the websocket is accessible at `127.0.0.1:2067/ws`. An example dev script to listen for rebuilds might look like this
```Javascript
let host = window.location.hostname;
const socket = new WebSocket('ws://' + host + ':2067/ws'); 

socket.addEventListener('message', (event) => {
    if (event.data === 'refresh') {
        async function testPage() {
            try {
            let res = await fetch(window.location.href)
            } catch (error) {
                console.error(error);
                setTimeout(testPage, 300);
                return;
            }
            window.location.reload();
        }

        testPage();
    }
});
```

## ZQDGR `new` scripts

With ZQDGR, you can easily initialize a new project with `zqdgr new` and optionally, you can specify a git repo to use as the initializer script. An example script is available at [github.com/juls0730/zqdgr-script-example](https://github.com/juls0730/zqdgr-script-example).

Every initialize script is expected to follow a few rules:

- The project must be a zqdgr project
- The `build` script must exist and must export a binary named `main`

ZQDGR passes your init script the directory that is being initialized in as the first and only argument

## Attribution

This project uses work from the following projects:

- [CompileDaemon](https://github.com/githubnemo/CompileDaemon)

  ```
    Copyright (c) 2013, Marian Tietz
    All rights reserved.

    Redistribution and use in source and binary forms, with or without
    modification, are permitted provided that the following conditions are met:

    * Redistributions of source code must retain the above copyright notice, this
      list of conditions and the following disclaimer.

    * Redistributions in binary form must reproduce the above copyright notice,
      this list of conditions and the following disclaimer in the documentation
      and/or other materials provided with the distribution.

    THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
    AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
    IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
    DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
    FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
    DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
    SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
    CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
    OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
    OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
  ```
