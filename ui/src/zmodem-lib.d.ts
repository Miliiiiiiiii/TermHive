// zmodem.js ships no TypeScript declarations; the library is used through a
// narrow surface (Sentry + session/offer methods) documented in its README,
// so a permissive module shape keeps the call sites honest without fighting
// upstream types.
declare module "zmodem.js" {
  const Zmodem: any;
  export = Zmodem;
}
