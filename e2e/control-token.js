// The e2e server's control token. It guards a throwaway data directory, so
// it is a fixed test value: the config hands it to the server and puts it in
// every context's om_control cookie, as a browser that used the bootstrap
// link would carry it. security.spec.js opens contexts without it.
const controlToken = 'e2e0'.repeat(16);

const authenticatedStorageState = {
  cookies: [{
    name: 'om_control',
    value: controlToken,
    domain: '127.0.0.1',
    path: '/',
    expires: -1,
    httpOnly: true,
    secure: false,
    sameSite: 'Strict',
  }],
  origins: [],
};

module.exports = { controlToken, authenticatedStorageState };
