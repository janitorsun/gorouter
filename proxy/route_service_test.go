package proxy_test

import (
	"bytes"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	"github.com/cloudfoundry/gorouter/common/secure"
	"github.com/cloudfoundry/gorouter/route_service"
	"github.com/cloudfoundry/gorouter/test_util"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Route Services", func() {
	var (
		routeServiceListener net.Listener
		routeServiceHandler  http.Handler
		signatureHeader      string
		metadataHeader       string
		cryptoKey            = "ABCDEFGHIJKLMNOP"
		forwardedUrl         string
	)

	JustBeforeEach(func() {
		var err error

		routeServiceListener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())

		tlsListener := newTlsListener(routeServiceListener)
		server := &http.Server{Handler: routeServiceHandler}
		go func() {
			err := server.Serve(tlsListener)
			Expect(err).ToNot(HaveOccurred())
		}()
	})

	BeforeEach(func() {
		conf.RouteServiceEnabled = true
		forwardedUrl = "http://my_host.com/resource+9-9_9?query=123&query$2=345#page1..5"

		routeServiceHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			metadataHeader := r.Header.Get(route_service.RouteServiceMetadata)
			signatureHeader := r.Header.Get(route_service.RouteServiceSignature)

			crypto, err := secure.NewAesGCM([]byte(cryptoKey))
			Expect(err).ToNot(HaveOccurred())
			_, err = route_service.SignatureFromHeaders(signatureHeader, metadataHeader, crypto)

			Expect(err).ToNot(HaveOccurred())
			Expect(r.Header.Get("X-CF-ApplicationID")).To(Equal(""))

			// validate client request header
			Expect(r.Header.Get("X-CF-Forwarded-Url")).To(Equal(forwardedUrl))

			w.Write([]byte("My Special Snowflake Route Service\n"))
		})
		crypto, err := secure.NewAesGCM([]byte(cryptoKey))
		Expect(err).ToNot(HaveOccurred())

		signature := &route_service.Signature{
			RequestedTime: time.Now(),
			ForwardedUrl:  forwardedUrl,
		}

		signatureHeader, metadataHeader, err = route_service.BuildSignatureAndMetadata(crypto, signature)
		Expect(err).ToNot(HaveOccurred())
	})

	Context("with Route Services disabled", func() {
		BeforeEach(func() {
			conf.RouteServiceEnabled = false
			conf.SSLSkipValidation = true
			routeServiceHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Fail("Should not get here into Route Service")
			})
		})

		It("return 502 Bad Gateway", func() {
			ln := registerHandlerWithRouteService(r, "my_host.com", "https://"+routeServiceListener.Addr().String(), func(conn *test_util.HttpConn) {
				Fail("Should not get here into the app")
			})
			defer ln.Close()

			conn := dialProxy(proxyServer)

			req := test_util.NewRequest("GET", "my_host.com", "/", nil)

			conn.WriteRequest(req)

			res, body := conn.ReadResponse()
			Expect(res.StatusCode).To(Equal(http.StatusBadGateway))
			Expect(body).To(ContainSubstring("Support for route services is disabled."))
		})
	})

	Context("with SSLSkipValidation enabled", func() {
		BeforeEach(func() {
			conf.SSLSkipValidation = true
		})

		Context("when a request does not have a valid Route service signature header", func() {
			It("redirects the request to the route service url", func() {
				ln := registerHandlerWithRouteService(r, "my_host.com", "https://"+routeServiceListener.Addr().String(), func(conn *test_util.HttpConn) {
					Fail("Should not get here")
				})
				defer ln.Close()

				conn := dialProxy(proxyServer)

				req := test_util.NewRequest("GET", "my_host.com", "/resource+9-9_9?query=123&query$2=345#page1..5", nil)

				conn.WriteRequest(req)

				res, body := conn.ReadResponse()
				Expect(res.StatusCode).To(Equal(http.StatusOK))
				Expect(body).To(ContainSubstring("My Special Snowflake Route Service"))
			})

			Context("when the route service is not available", func() {
				It("returns a 502 bad gateway error", func() {
					ln := registerHandlerWithRouteService(r, "my_host.com", "https://bad-route-service", func(conn *test_util.HttpConn) {
						Fail("Should not get here")
					})
					defer ln.Close()

					conn := dialProxy(proxyServer)

					req := test_util.NewRequest("GET", "my_host.com", "/resource+9-9_9?query=123&query$2=345#page1..5", nil)

					conn.WriteRequest(req)

					res, _ := conn.ReadResponse()
					Expect(res.StatusCode).To(Equal(http.StatusBadGateway))
					// Expect(body).To(ContainSubstring("My Special Snowflake Route Service"))
				})
			})
		})

		Context("when a request has a valid Route service signature header", func() {
			BeforeEach(func() {
				routeServiceHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					Fail("Should not get here into Route Service")
				})
			})

			It("routes to the backend instance and strips headers", func() {
				ln := registerHandlerWithRouteService(r, "test/my_path", "https://"+routeServiceListener.Addr().String(), func(conn *test_util.HttpConn) {
					req, _ := conn.ReadRequest()
					Expect(req.Header.Get(route_service.RouteServiceSignature)).To(Equal(""))
					Expect(req.Header.Get(route_service.RouteServiceSignature)).To(Equal(""))

					out := &bytes.Buffer{}
					out.WriteString("backend instance")
					res := &http.Response{
						StatusCode: http.StatusOK,
						Body:       ioutil.NopCloser(out),
					}
					conn.WriteResponse(res)
				})
				defer ln.Close()

				conn := dialProxy(proxyServer)

				req := test_util.NewRequest("GET", "test", "/my_path", nil)
				req.Header.Set(route_service.RouteServiceSignature, signatureHeader)
				req.Header.Set(route_service.RouteServiceMetadata, metadataHeader)
				req.Header.Set(route_service.RouteServiceForwardedUrl, forwardedUrl)
				conn.WriteRequest(req)

				res, body := conn.ReadResponse()
				Expect(res.StatusCode).To(Equal(http.StatusOK))
				Expect(body).To(ContainSubstring("backend instance"))
			})

			Context("and is forwarding to a route service on CF", func() {
				It("does not strip the signature header", func() {
					ln := registerHandler(r, "test/my_path", func(conn *test_util.HttpConn) {
						req, _ := conn.ReadRequest()
						Expect(req.Header.Get(route_service.RouteServiceSignature)).To(Equal("some-signature"))

						out := &bytes.Buffer{}
						out.WriteString("route service instance")
						res := &http.Response{
							StatusCode: http.StatusOK,
							Body:       ioutil.NopCloser(out),
						}
						conn.WriteResponse(res)
					})
					defer ln.Close()

					conn := dialProxy(proxyServer)

					req := test_util.NewRequest("GET", "test", "/my_path", nil)
					req.Header.Set(route_service.RouteServiceSignature, "some-signature")
					conn.WriteRequest(req)

					res, body := conn.ReadResponse()
					Expect(res.StatusCode).To(Equal(http.StatusOK))
					Expect(body).To(ContainSubstring("route service instance"))
				})
			})

			It("returns 502 when backend not available", func() {
				ip, err := net.ResolveTCPAddr("tcp", "localhost:81")
				Expect(err).To(BeNil())

				// register route service, should NOT route to it
				registerAddr(r, "mybadapp.com", "https://"+routeServiceListener.Addr().String(), ip, "instanceId")

				conn := dialProxy(proxyServer)

				req := test_util.NewRequest("GET", "mybadapp.com", "/", nil)
				req.Header.Set(route_service.RouteServiceSignature, signatureHeader)
				req.Header.Set(route_service.RouteServiceMetadata, metadataHeader)
				req.Header.Set(route_service.RouteServiceForwardedUrl, forwardedUrl)
				conn.WriteRequest(req)
				resp, _ := conn.ReadResponse()

				Expect(resp.StatusCode).To(Equal(http.StatusBadGateway))
			})
		})
	})

	Context("when a request has a signature header but no metadata header", func() {
		It("returns a bad request error", func() {
			ln := registerHandlerWithRouteService(r, "test/my_path", "https://expired.com", func(conn *test_util.HttpConn) {
				Fail("Should not get here")
			})
			defer ln.Close()
			conn := dialProxy(proxyServer)

			req := test_util.NewRequest("GET", "test", "/my_path", nil)
			req.Header.Set(route_service.RouteServiceSignature, signatureHeader)
			conn.WriteRequest(req)

			res, body := conn.ReadResponse()
			Expect(res.StatusCode).To(Equal(http.StatusBadRequest))
			Expect(body).To(ContainSubstring("Failed to validate Route Service Signature"))
		})
	})

	Context("when a request has an expired Route service signature header", func() {
		BeforeEach(func() {
			signatureHeader = "zKQt4bnxW30KxpGUH-saDxTIG98RbKx7tLkyaDBNdE_vTZletyba3bN2yOw9SLtgUhEVsLq3zLYe-7tngGP5edbybGwiF0A6"
			metadataHeader = "eyJpdiI6IjlBVnBiZWRIdUZMbU1KaVciLCJub25jZSI6InpWdHM5aU1RdXNVV2U5UkoifQ=="
		})

		It("returns an route service request expired error", func() {
			ln := registerHandlerWithRouteService(r, "test/my_path", "https://expired.com", func(conn *test_util.HttpConn) {
				Fail("Should not get here")
			})
			defer ln.Close()
			conn := dialProxy(proxyServer)

			req := test_util.NewRequest("GET", "test", "/my_path", nil)
			req.Header.Set(route_service.RouteServiceSignature, signatureHeader)
			req.Header.Set(route_service.RouteServiceMetadata, metadataHeader)
			conn.WriteRequest(req)

			res, body := conn.ReadResponse()
			Expect(res.StatusCode).To(Equal(http.StatusBadRequest))
			Expect(body).To(ContainSubstring("Failed to validate Route Service Signature"))
		})
	})

	Context("when a route service modifies the X-CF-Forwarded-Url header", func() {
		It("returns a bad request error", func() {
			ln := registerHandlerWithRouteService(r, "test/my_path", "https://rs.com", func(conn *test_util.HttpConn) {
				Fail("Should not get here")
			})
			defer ln.Close()
			conn := dialProxy(proxyServer)

			req := test_util.NewRequest("GET", "test", "/my_path", nil)
			req.Header.Set(route_service.RouteServiceSignature, signatureHeader)
			req.Header.Set(route_service.RouteServiceMetadata, metadataHeader)
			req.Header.Set(route_service.RouteServiceForwardedUrl, "some-other-url")
			conn.WriteRequest(req)

			res, body := conn.ReadResponse()
			Expect(res.StatusCode).To(Equal(http.StatusBadRequest))
			Expect(body).To(ContainSubstring("Failed to validate Route Service Signature"))
		})
	})

	Context("when a route service strips off the X-CF-Forwarded-Url header", func() {
		It("returns a bad request error", func() {
			ln := registerHandlerWithRouteService(r, "test/my_path", "https://rs.com", func(conn *test_util.HttpConn) {
				Fail("Should not get here")
			})
			defer ln.Close()
			conn := dialProxy(proxyServer)

			req := test_util.NewRequest("GET", "test", "/my_path", nil)
			req.Header.Set(route_service.RouteServiceSignature, signatureHeader)
			req.Header.Set(route_service.RouteServiceMetadata, metadataHeader)
			conn.WriteRequest(req)

			res, body := conn.ReadResponse()
			Expect(res.StatusCode).To(Equal(http.StatusBadRequest))
			Expect(body).To(ContainSubstring("Failed to validate Route Service Signature"))
		})
	})

	Context("when the header key does not match the current crypto key in the configuration", func() {
		BeforeEach(func() {
			// Change the current key to make the header key not match the current key.
			var err error
			crypto, err = secure.NewAesGCM([]byte("QRSTUVWXYZ123456"))
			Expect(err).NotTo(HaveOccurred())
		})

		Context("when there is no previous key in the configuration", func() {
			It("rejects the signature", func() {
				ln := registerHandlerWithRouteService(r, "test/my_path", "https://badkey.com", func(conn *test_util.HttpConn) {
					Fail("Should not get here")
				})
				defer ln.Close()

				conn := dialProxy(proxyServer)
				req := test_util.NewRequest("GET", "test", "/my_path", nil)
				req.Header.Set(route_service.RouteServiceSignature, signatureHeader)
				req.Header.Set(route_service.RouteServiceMetadata, metadataHeader)
				conn.WriteRequest(req)

				res, body := conn.ReadResponse()
				Expect(res.StatusCode).To(Equal(http.StatusBadRequest))
				Expect(body).To(ContainSubstring("Failed to validate Route Service Signature"))
			})
		})

		Context("when the header key matches the previous key in the configuration", func() {
			BeforeEach(func() {
				var err error
				cryptoPrev, err = secure.NewAesGCM([]byte(cryptoKey))
				Expect(err).NotTo(HaveOccurred())
			})

			It("forwards the request to the application", func() {
				ln := registerHandlerWithRouteService(r, "test/my_path", "https://"+routeServiceListener.Addr().String(), func(conn *test_util.HttpConn) {
					conn.ReadRequest()

					out := &bytes.Buffer{}
					out.WriteString("backend instance")
					res := &http.Response{
						StatusCode: http.StatusOK,
						Body:       ioutil.NopCloser(out),
					}
					conn.WriteResponse(res)
				})

				defer ln.Close()

				conn := dialProxy(proxyServer)
				req := test_util.NewRequest("GET", "test", "/my_path", nil)
				req.Header.Set(route_service.RouteServiceSignature, signatureHeader)
				req.Header.Set(route_service.RouteServiceMetadata, metadataHeader)
				conn.WriteRequest(req)

				res, body := conn.ReadResponse()
				Expect(res.StatusCode).To(Equal(http.StatusOK))
				Expect(body).To(ContainSubstring("backend instance"))
			})

			Context("when a request has an expired Route service signature header", func() {
				BeforeEach(func() {
					signature := &route_service.Signature{
						RequestedTime: time.Now().Add(-10 * time.Hour),
						ForwardedUrl:  forwardedUrl,
					}
					signatureHeader, metadataHeader, _ = route_service.BuildSignatureAndMetadata(crypto, signature)
				})

				It("returns an route service request expired error", func() {
					ln := registerHandlerWithRouteService(r, "test/my_path", "https://expired.com", func(conn *test_util.HttpConn) {
						Fail("Should not get here")
					})
					defer ln.Close()
					conn := dialProxy(proxyServer)

					req := test_util.NewRequest("GET", "test", "/my_path", nil)
					req.Header.Set(route_service.RouteServiceSignature, signatureHeader)
					req.Header.Set(route_service.RouteServiceMetadata, metadataHeader)
					req.Header.Set(route_service.RouteServiceForwardedUrl, forwardedUrl)
					conn.WriteRequest(req)

					res, body := conn.ReadResponse()
					Expect(res.StatusCode).To(Equal(http.StatusBadRequest))
					Expect(body).To(ContainSubstring("Failed to validate Route Service Signature"))
				})
			})
		})

		Context("when the header key does not match the previous key in the configuration", func() {
			BeforeEach(func() {
				var err error
				cryptoPrev, err = secure.NewAesGCM([]byte("QRSTUVWXYZ123456"))
				Expect(err).NotTo(HaveOccurred())
			})

			It("rejects the signature", func() {
				ln := registerHandlerWithRouteService(r, "test/my_path", "https://badkey.com", func(conn *test_util.HttpConn) {
					Fail("Should not get here")
				})
				defer ln.Close()

				conn := dialProxy(proxyServer)
				req := test_util.NewRequest("GET", "test", "/my_path", nil)
				req.Header.Set(route_service.RouteServiceSignature, signatureHeader)
				req.Header.Set(route_service.RouteServiceMetadata, metadataHeader)
				conn.WriteRequest(req)

				res, body := conn.ReadResponse()

				Expect(res.StatusCode).To(Equal(http.StatusBadRequest))
				Expect(body).To(ContainSubstring("Failed to validate Route Service Signature"))
			})
		})
	})

	It("returns an error when a bad route service url is used", func() {
		ln := registerHandlerWithRouteService(r, "test/my_path", "https://bad%20hostname.com", func(conn *test_util.HttpConn) {
			Fail("Should not get here")
		})
		defer ln.Close()

		conn := dialProxy(proxyServer)

		req := test_util.NewRequest("GET", "test", "/my_path", nil)
		conn.WriteRequest(req)

		res, body := readResponse(conn)

		Expect(res.StatusCode).To(Equal(http.StatusInternalServerError))
		Expect(body).NotTo(ContainSubstring("My Special Snowflake Route Service"))
	})

	It("returns a 502 when the SSL cert of the route service is signed by an unknown authority", func() {
		ln := registerHandlerWithRouteService(r, "test/my_path", "https://"+routeServiceListener.Addr().String(), func(conn *test_util.HttpConn) {
			Fail("Should not get here")
		})
		defer ln.Close()

		conn := dialProxy(proxyServer)

		req := test_util.NewRequest("GET", "test", "/my_path", nil)
		conn.WriteRequest(req)

		res, _ := readResponse(conn)

		Expect(res.StatusCode).To(Equal(http.StatusBadGateway))
	})

	It("returns a 200 when we route to a route service that has a valid cert", func() {
		// sorry google we are using you
		ln := registerHandlerWithRouteService(r, "test/my_path", "https://www.google.com", func(conn *test_util.HttpConn) {
			Fail("Should not get here")
		})
		defer ln.Close()

		conn := dialProxy(proxyServer)

		req := test_util.NewRequest("GET", "test", "/my_path", nil)
		conn.WriteRequest(req)

		res, _ := readResponse(conn)

		okCodes := []int{http.StatusOK, http.StatusFound}
		Expect(okCodes).Should(ContainElement(res.StatusCode))
	})
})
