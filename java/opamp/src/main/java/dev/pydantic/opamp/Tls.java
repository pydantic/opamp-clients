// Copyright (c) 2026 Bill Easton. All rights reserved.
// SPDX-License-Identifier: LicenseRef-All-Rights-Reserved

package dev.pydantic.opamp;

import java.io.ByteArrayInputStream;
import java.net.Socket;
import java.security.GeneralSecurityException;
import java.security.KeyFactory;
import java.security.KeyStore;
import java.security.PrivateKey;
import java.security.PublicKey;
import java.security.Signature;
import java.security.cert.CertificateException;
import java.security.cert.CertificateFactory;
import java.security.cert.X509Certificate;
import java.security.interfaces.RSAPrivateCrtKey;
import java.security.interfaces.RSAPublicKey;
import java.security.spec.PKCS8EncodedKeySpec;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Base64;
import java.util.List;
import java.util.Objects;
import java.util.regex.Matcher;
import java.util.regex.Pattern;
import javax.net.ssl.KeyManager;
import javax.net.ssl.KeyManagerFactory;
import javax.net.ssl.SSLContext;
import javax.net.ssl.SSLEngine;
import javax.net.ssl.TrustManager;
import javax.net.ssl.TrustManagerFactory;
import javax.net.ssl.X509ExtendedTrustManager;

/**
 * TLS settings for {@code https://} and {@code wss://} servers and for package downloads: the CA
 * certificates to trust, a client certificate to present, and the TLS versions allowed. Everything
 * is PEM text; certificates and keys are read when given, so a bad one fails there.
 *
 * <pre>{@code
 * Tls tls = Tls.builder()
 *     .trustedCa(Files.readString(Path.of("ca.pem")))
 *     .clientCertificate(Files.readString(Path.of("agent.pem")), Files.readString(Path.of("agent-key.pem")))
 *     .build();
 * }</pre>
 */
public final class Tls {
  /** A TLS version. Nothing older than 1.2. */
  public enum Version {
    TLS_1_2("TLSv1.2"),
    TLS_1_3("TLSv1.3");

    final String protocol;

    Version(String protocol) {
      this.protocol = protocol;
    }
  }

  private final List<X509Certificate> trusted;
  private final boolean systemRoots;
  private final List<X509Certificate> chain;
  private final PrivateKey key;
  private final Version min;
  private final Version max;

  private Tls(Builder b) {
    trusted = List.copyOf(b.trusted);
    systemRoots = b.systemRoots;
    chain = b.chain;
    key = b.key;
    min = b.min;
    max = b.max;
  }

  /** Settings to fill in. With none, the client trusts the system's CAs and allows TLS 1.2 and 1.3. */
  public static Builder builder() {
    return new Builder();
  }

  /** Builds {@link Tls}. */
  public static final class Builder {
    private final List<X509Certificate> trusted = new ArrayList<>();
    private boolean systemRoots;
    private List<X509Certificate> chain = List.of();
    private PrivateKey key;
    private Version min = Version.TLS_1_2;
    private Version max = Version.TLS_1_3;

    private Builder() {}

    /**
     * Trust the CA certificates in {@code pem} (one or more). The server's certificate must then
     * chain to one of them: the system's CAs are no longer trusted, unless {@link
     * #includeSystemRoots}.
     */
    public Builder trustedCa(String pem) {
      List<X509Certificate> certs = certificates(pem);
      if (certs.isEmpty()) {
        throw new IllegalArgumentException("no CERTIFICATE in the CA PEM");
      }
      trusted.addAll(certs);
      return this;
    }

    /** Trust the system's CAs as well as those given by {@link #trustedCa}; default false. */
    public Builder includeSystemRoots(boolean include) {
      this.systemRoots = include;
      return this;
    }

    /**
     * Present this certificate when the server asks for one. {@code certPem}: the certificate,
     * then any intermediates. {@code keyPem}: its private key, not encrypted, in any of the three
     * PEM forms: PKCS #8 ({@code BEGIN PRIVATE KEY}), PKCS #1 RSA ({@code BEGIN RSA PRIVATE KEY})
     * or SEC 1 EC ({@code BEGIN EC PRIVATE KEY}, with its curve named). It may be in the same PEM
     * as the certificate. An encrypted key ({@code BEGIN ENCRYPTED PRIVATE KEY}) is refused:
     * decrypt it first ({@code openssl pkcs8 -topk8 -nocrypt}). The key must be the first
     * certificate's: a key that doesn't match is refused here.
     */
    public Builder clientCertificate(String certPem, String keyPem) {
      List<X509Certificate> certs = certificates(certPem);
      if (certs.isEmpty()) {
        throw new IllegalArgumentException("no CERTIFICATE in the client certificate PEM");
      }
      PrivateKey k = privateKey(keyPem);
      checkPair(certs.get(0), k);
      this.key = k;
      this.chain = List.copyOf(certs);
      return this;
    }

    /** The oldest TLS version allowed; default 1.2. */
    public Builder minVersion(Version version) {
      this.min = Objects.requireNonNull(version);
      return this;
    }

    /** The newest TLS version allowed; default 1.3. */
    public Builder maxVersion(Version version) {
      this.max = Objects.requireNonNull(version);
      return this;
    }

    /** The settings. */
    public Tls build() {
      if (min.compareTo(max) > 0) {
        throw new IllegalArgumentException("TLS minimum version " + min + " is newer than the maximum " + max);
      }
      return new Tls(this);
    }
  }

  /** The protocols allowed, newest first: for {@code SSLParameters.setProtocols}. */
  String[] protocols() {
    Version[] all = Version.values();
    List<String> out = new ArrayList<>();
    for (int i = max.ordinal(); i >= min.ordinal(); i--) {
      out.add(all[i].protocol);
    }
    return out.toArray(new String[0]);
  }

  /** An SSL context that presents the client certificate, if any, and trusts what was asked. */
  SSLContext context() {
    try {
      KeyManager[] kms = null;
      if (key != null) {
        char[] password = "opamp".toCharArray();
        KeyStore ks = KeyStore.getInstance("PKCS12");
        ks.load(null, null);
        ks.setKeyEntry("client", key, password, chain.toArray(new X509Certificate[0]));
        KeyManagerFactory kmf = KeyManagerFactory.getInstance(KeyManagerFactory.getDefaultAlgorithm());
        kmf.init(ks, password);
        kms = kmf.getKeyManagers();
      }
      TrustManager[] tms = null; // the system's
      if (!trusted.isEmpty()) {
        KeyStore ks = KeyStore.getInstance("PKCS12");
        ks.load(null, null);
        for (int i = 0; i < trusted.size(); i++) {
          ks.setCertificateEntry("ca" + i, trusted.get(i));
        }
        X509ExtendedTrustManager custom = trustManager(ks);
        tms = new TrustManager[] {systemRoots ? new Either(custom, trustManager(null)) : custom};
      }
      SSLContext ctx = SSLContext.getInstance("TLS");
      ctx.init(kms, tms, null);
      return ctx;
    } catch (GeneralSecurityException | java.io.IOException e) {
      throw new IllegalStateException("building the TLS context: " + e.getMessage(), e);
    }
  }

  private static X509ExtendedTrustManager trustManager(KeyStore ks) throws GeneralSecurityException {
    TrustManagerFactory tmf = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm());
    tmf.init(ks);
    for (TrustManager tm : tmf.getTrustManagers()) {
      if (tm instanceof X509ExtendedTrustManager x) {
        return x;
      }
    }
    throw new GeneralSecurityException("no X.509 trust manager");
  }

  private static final Pattern PEM = Pattern.compile("-----BEGIN ([A-Z0-9 ]+)-----(.*?)-----END \\1-----", Pattern.DOTALL);

  /** The DER of each PEM block, with its label. */
  private record Block(String label, byte[] der) {}

  private static List<Block> blocks(String pem) {
    List<Block> out = new ArrayList<>();
    Matcher m = PEM.matcher(Objects.requireNonNull(pem));
    while (m.find()) {
      try {
        out.add(new Block(m.group(1), Base64.getMimeDecoder().decode(m.group(2))));
      } catch (IllegalArgumentException e) {
        throw new IllegalArgumentException("bad base64 in a PEM " + m.group(1) + " block", e);
      }
    }
    return out;
  }

  static List<X509Certificate> certificates(String pem) {
    List<X509Certificate> out = new ArrayList<>();
    try {
      CertificateFactory cf = CertificateFactory.getInstance("X.509");
      for (Block b : blocks(pem)) {
        if (b.label.equals("CERTIFICATE")) {
          out.add((X509Certificate) cf.generateCertificate(new ByteArrayInputStream(b.der)));
        }
      }
    } catch (CertificateException e) {
      throw new IllegalArgumentException("bad certificate in PEM: " + e.getMessage(), e);
    }
    return out;
  }

  static PrivateKey privateKey(String pem) {
    for (Block b : blocks(pem)) {
      switch (b.label) {
        // The JDK reads PKCS #8 only: the other two forms are wrapped as PKCS #8 first.
        case "RSA PRIVATE KEY" -> {
          return pkcs8(RSA_ENCRYPTION, b.der, "RSA");
        }
        case "EC PRIVATE KEY" -> {
          return pkcs8(der(0x30, cat(EC_PUBLIC_KEY, sec1Curve(b.der))), b.der, "EC");
        }
        case "PRIVATE KEY" -> {
          PKCS8EncodedKeySpec spec = new PKCS8EncodedKeySpec(b.der);
          for (String alg : List.of("RSA", "EC", "Ed25519", "Ed448", "RSASSA-PSS")) {
            try {
              return KeyFactory.getInstance(alg).generatePrivate(spec);
            } catch (GeneralSecurityException e) {
              // not this algorithm
            }
          }
          throw new IllegalArgumentException("unsupported PKCS #8 private key (RSA, EC, Ed25519 and Ed448 are)");
        }
        case "ENCRYPTED PRIVATE KEY" -> throw new IllegalArgumentException(
            "the private key is encrypted: give it decrypted, PKCS #8 (openssl pkcs8 -topk8 -nocrypt)");
        default -> {} // a certificate, parameters: not the key
      }
    }
    throw new IllegalArgumentException("no PRIVATE KEY in the key PEM");
  }

  /** The AlgorithmIdentifier of an RSA key: rsaEncryption (1.2.840.113549.1.1.1), NULL parameters. */
  private static final byte[] RSA_ENCRYPTION = {0x30, 0x0d, 0x06, 0x09, 0x2a, (byte) 0x86, 0x48, (byte) 0x86,
      (byte) 0xf7, 0x0d, 0x01, 0x01, 0x01, 0x05, 0x00};
  /** The OID id-ecPublicKey (1.2.840.10045.2.1), an EC key's algorithm. */
  private static final byte[] EC_PUBLIC_KEY = {0x06, 0x07, 0x2a, (byte) 0x86, 0x48, (byte) 0xce, 0x3d, 0x02, 0x01};

  /** {@code key} (PKCS #1 or SEC 1 DER) as PKCS #8: version 0, {@code algorithm}, the key. */
  private static PrivateKey pkcs8(byte[] algorithm, byte[] key, String alg) {
    byte[] info = der(0x30, cat(new byte[] {0x02, 0x01, 0x00}, algorithm, der(0x04, key)));
    try {
      return KeyFactory.getInstance(alg).generatePrivate(new PKCS8EncodedKeySpec(info));
    } catch (GeneralSecurityException e) {
      throw new IllegalArgumentException("unreadable " + alg + " private key: " + e.getMessage(), e);
    }
  }

  /**
   * The curve's OID (its whole DER) from a SEC 1 ECPrivateKey: SEQUENCE { version, privateKey,
   * [0] parameters, [1] publicKey }. A key without its curve can't be read.
   */
  private static byte[] sec1Curve(byte[] der) {
    try {
      int[] seq = tlv(der, 0);
      if (der[0] != 0x30) {
        throw new IllegalArgumentException("not a sequence");
      }
      for (int at = seq[0]; at < seq[0] + seq[1]; ) {
        int[] t = tlv(der, at);
        if ((der[at] & 0xff) == 0xa0) {
          int[] oid = tlv(der, t[0]);
          if (der[t[0]] != 0x06) {
            throw new IllegalArgumentException("its parameters aren't a named curve");
          }
          return Arrays.copyOfRange(der, t[0], oid[0] + oid[1]);
        }
        at = t[0] + t[1];
      }
    } catch (ArrayIndexOutOfBoundsException e) {
      throw new IllegalArgumentException("unreadable EC PRIVATE KEY", e);
    }
    throw new IllegalArgumentException("the EC PRIVATE KEY doesn't name its curve");
  }

  /** The DER element at {@code at}: where its contents start, and their length. */
  private static int[] tlv(byte[] der, int at) {
    int len = der[at + 1] & 0xff;
    int start = at + 2;
    if (len >= 0x80) {
      int n = len & 0x7f;
      if (n == 0 || n > 3) {
        throw new IllegalArgumentException("a DER length of " + n + " bytes");
      }
      len = 0;
      for (int i = 0; i < n; i++) {
        len = (len << 8) | (der[start + i] & 0xff);
      }
      start += n;
    }
    if (start + len > der.length) {
      throw new IllegalArgumentException("a DER element runs past the end");
    }
    return new int[] {start, len};
  }

  /** A DER element: {@code tag}, the length, {@code contents}. */
  private static byte[] der(int tag, byte[] contents) {
    int n = contents.length;
    byte[] len = n < 0x80 ? new byte[] {(byte) n}
        : n < 0x100 ? new byte[] {(byte) 0x81, (byte) n}
        : n < 0x10000 ? new byte[] {(byte) 0x82, (byte) (n >> 8), (byte) n}
        : new byte[] {(byte) 0x83, (byte) (n >> 16), (byte) (n >> 8), (byte) n};
    return cat(new byte[] {(byte) tag}, len, contents);
  }

  private static byte[] cat(byte[]... parts) {
    java.io.ByteArrayOutputStream out = new java.io.ByteArrayOutputStream();
    for (byte[] p : parts) {
      out.writeBytes(p);
    }
    return out.toByteArray();
  }

  /**
   * Refuse a key that isn't the certificate's: compare the public keys (RSA), or sign with the key
   * and verify with the certificate (other algorithms, whose public key the JDK can't derive).
   */
  static void checkPair(X509Certificate cert, PrivateKey key) {
    PublicKey pub = cert.getPublicKey();
    String mismatch = "the private key does not match the client certificate (" + cert.getSubjectX500Principal().getName() + ")";
    if (!pub.getAlgorithm().equals(key.getAlgorithm())) {
      throw new IllegalArgumentException(mismatch + ": the key is " + key.getAlgorithm() + ", the certificate's is " + pub.getAlgorithm());
    }
    if (key instanceof RSAPrivateCrtKey k && pub instanceof RSAPublicKey p) {
      if (!k.getModulus().equals(p.getModulus()) || !k.getPublicExponent().equals(p.getPublicExponent())) {
        throw new IllegalArgumentException(mismatch);
      }
      return;
    }
    String alg = switch (key.getAlgorithm()) {
      case "RSA" -> "SHA256withRSA";
      case "EC" -> "SHA256withECDSA";
      default -> key.getAlgorithm(); // EdDSA, Ed25519, Ed448
    };
    try {
      byte[] data = "opamp key check".getBytes(java.nio.charset.StandardCharsets.US_ASCII);
      Signature sign = Signature.getInstance(alg);
      sign.initSign(key);
      sign.update(data);
      byte[] sig = sign.sign();
      Signature verify = Signature.getInstance(alg);
      verify.initVerify(pub);
      verify.update(data);
      if (!verify.verify(sig)) {
        throw new IllegalArgumentException(mismatch);
      }
    } catch (GeneralSecurityException e) {
      throw new IllegalArgumentException(mismatch + ": " + e.getMessage(), e);
    }
  }

  /** Trusts what either trust manager trusts: the given CAs first, then the system's. */
  private static final class Either extends X509ExtendedTrustManager {
    private final X509ExtendedTrustManager first;
    private final X509ExtendedTrustManager second;

    Either(X509ExtendedTrustManager first, X509ExtendedTrustManager second) {
      this.first = first;
      this.second = second;
    }

    private interface Check {
      void on(X509ExtendedTrustManager tm) throws CertificateException;
    }

    private void either(Check c) throws CertificateException {
      try {
        c.on(first);
      } catch (CertificateException e) {
        try {
          c.on(second);
        } catch (CertificateException e2) {
          e.addSuppressed(e2);
          throw e;
        }
      }
    }

    @Override
    public void checkServerTrusted(X509Certificate[] chain, String authType, SSLEngine engine) throws CertificateException {
      either(tm -> tm.checkServerTrusted(chain, authType, engine));
    }

    @Override
    public void checkServerTrusted(X509Certificate[] chain, String authType, Socket socket) throws CertificateException {
      either(tm -> tm.checkServerTrusted(chain, authType, socket));
    }

    @Override
    public void checkServerTrusted(X509Certificate[] chain, String authType) throws CertificateException {
      either(tm -> tm.checkServerTrusted(chain, authType));
    }

    @Override
    public void checkClientTrusted(X509Certificate[] chain, String authType, SSLEngine engine) throws CertificateException {
      either(tm -> tm.checkClientTrusted(chain, authType, engine));
    }

    @Override
    public void checkClientTrusted(X509Certificate[] chain, String authType, Socket socket) throws CertificateException {
      either(tm -> tm.checkClientTrusted(chain, authType, socket));
    }

    @Override
    public void checkClientTrusted(X509Certificate[] chain, String authType) throws CertificateException {
      either(tm -> tm.checkClientTrusted(chain, authType));
    }

    @Override
    public X509Certificate[] getAcceptedIssuers() {
      X509Certificate[] a = first.getAcceptedIssuers();
      X509Certificate[] b = second.getAcceptedIssuers();
      X509Certificate[] out = Arrays.copyOf(a, a.length + b.length);
      System.arraycopy(b, 0, out, a.length, b.length);
      return out;
    }
  }
}
