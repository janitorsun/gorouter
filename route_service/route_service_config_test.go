package route_service_test

import (
	"net/http"
	"net/url"
	"time"

	"github.com/cloudfoundry/gorouter/common/secure"
	"github.com/cloudfoundry/gorouter/route_service"
	"github.com/cloudfoundry/gorouter/test_util"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Route Service Config", func() {
	var (
		config     *route_service.RouteServiceConfig
		crypto     secure.Crypto
		cryptoPrev secure.Crypto
		cryptoKey  = "ABCDEFGHIJKLMNOP"
	)

	BeforeEach(func() {
		var err error
		crypto, err = secure.NewAesGCM([]byte(cryptoKey))
		Expect(err).ToNot(HaveOccurred())
		config = route_service.NewRouteServiceConfig(true, 1*time.Hour, crypto, cryptoPrev)
	})

	AfterEach(func() {
		crypto = nil
		cryptoPrev = nil
		config = nil
	})

	Describe("SetupRouteServiceRequest", func() {
		var (
			request *http.Request
			rsArgs  route_service.RouteServiceArgs
		)

		BeforeEach(func() {
			request = test_util.NewRequest("GET", "test.com", "/path/", nil)
			str := "https://example-route-service.com"
			parsed, err := url.Parse(str)
			Expect(err).NotTo(HaveOccurred())
			rsArgs = route_service.RouteServiceArgs{
				UrlString:       str,
				ParsedUrl:       parsed,
				Signature:       "signature",
				Metadata:        "metadata",
				ForwardedUrlRaw: "http://test.com/path/",
			}
		})

		It("sets the signature and metadata headers", func() {
			Expect(request.Header.Get(route_service.RouteServiceSignature)).To(Equal(""))
			Expect(request.Header.Get(route_service.RouteServiceMetadata)).To(Equal(""))

			config.SetupRouteServiceRequest(request, rsArgs)

			Expect(request.Header.Get(route_service.RouteServiceSignature)).To(Equal("signature"))
			Expect(request.Header.Get(route_service.RouteServiceMetadata)).To(Equal("metadata"))
		})

		It("sets the forwarded URL header", func() {
			Expect(request.Header.Get(route_service.RouteServiceForwardedUrl)).To(Equal(""))

			config.SetupRouteServiceRequest(request, rsArgs)

			Expect(request.Header.Get(route_service.RouteServiceForwardedUrl)).To(Equal("http://test.com/path/"))
		})

		It("changes the request host and URL", func() {
			config.SetupRouteServiceRequest(request, rsArgs)

			Expect(request.URL.Host).To(Equal("example-route-service.com"))
			Expect(request.URL.Scheme).To(Equal("https"))
		})
	})

	Describe("ValidateSignature", func() {
		var (
			signatureHeader string
			metadataHeader  string
			headers         *http.Header
			signature       *route_service.Signature
		)

		BeforeEach(func() {
			h := make(http.Header, 0)
			headers = &h
			var err error

			signature = &route_service.Signature{
				RequestedTime: time.Now(),
				ForwardedUrl:  "some-forwarded-url",
			}
			signatureHeader, metadataHeader, err = route_service.BuildSignatureAndMetadata(crypto, signature)
			Expect(err).ToNot(HaveOccurred())

			headers.Set(route_service.RouteServiceForwardedUrl, "some-forwarded-url")
		})

		JustBeforeEach(func() {
			headers.Set(route_service.RouteServiceSignature, signatureHeader)
			headers.Set(route_service.RouteServiceMetadata, metadataHeader)
		})

		It("decrypts a valid signature", func() {
			err := config.ValidateSignature(headers)
			Expect(err).NotTo(HaveOccurred())
		})

		Context("when the timestamp is expired", func() {
			BeforeEach(func() {
				signature = &route_service.Signature{
					RequestedTime: time.Now().Add(-10 * time.Hour),
					ForwardedUrl:  "some-forwarded-url",
				}
				var err error
				signatureHeader, metadataHeader, err = route_service.BuildSignatureAndMetadata(crypto, signature)
				Expect(err).ToNot(HaveOccurred())
			})

			It("returns an route service request expired error", func() {
				err := config.ValidateSignature(headers)
				Expect(err).To(HaveOccurred())
				Expect(err).To(BeAssignableToTypeOf(route_service.RouteServiceExpired))
				Expect(err.Error()).To(ContainSubstring("request expired"))
			})
		})

		Context("when the signature is invalid", func() {
			BeforeEach(func() {
				signatureHeader = "zKQt4bnxW30Kxky"
				metadataHeader = "eyJpdiI6IjlBVn"
			})
			It("returns an error", func() {
				err := config.ValidateSignature(headers)
				Expect(err).To(HaveOccurred())
			})
		})

		Context("when the X-CF-Forwarded-Url is missing", func() {
			BeforeEach(func() {
				headers.Del(route_service.RouteServiceForwardedUrl)
			})

			It("returns a route service request bad forwarded url error", func() {
				err := config.ValidateSignature(headers)
				Expect(err).To(HaveOccurred())
				Expect(err).To(BeAssignableToTypeOf(route_service.RouteServiceForwardedUrlMismatch))
			})
		})

		Context("when the X-CF-Forwarded-Url is different from the signature", func() {
			BeforeEach(func() {
				headers.Set(route_service.RouteServiceForwardedUrl, "some-other-url")
			})

			It("returns a route service request bad forwarded url error", func() {
				err := config.ValidateSignature(headers)
				Expect(err).To(HaveOccurred())
				Expect(err).To(BeAssignableToTypeOf(route_service.RouteServiceForwardedUrlMismatch))
			})
		})

		Context("when the header does not match the current key", func() {
			BeforeEach(func() {
				var err error
				crypto, err = secure.NewAesGCM([]byte("QRSTUVWXYZ123456"))
				Expect(err).NotTo(HaveOccurred())
				config = route_service.NewRouteServiceConfig(true, 1*time.Hour, crypto, cryptoPrev)
			})

			Context("when there is no previous key in the configuration", func() {
				It("rejects the signature", func() {
					err := config.ValidateSignature(headers)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("authentication failed"))
				})
			})

			Context("when the header key matches the previous key in the configuration", func() {
				BeforeEach(func() {
					var err error
					cryptoPrev, err = secure.NewAesGCM([]byte(cryptoKey))
					Expect(err).ToNot(HaveOccurred())
					config = route_service.NewRouteServiceConfig(true, 1*time.Hour, crypto, cryptoPrev)
				})

				It("validates the signature", func() {
					err := config.ValidateSignature(headers)
					Expect(err).NotTo(HaveOccurred())
				})

				Context("when a request has an expired Route service signature header", func() {
					BeforeEach(func() {
						signature = &route_service.Signature{
							RequestedTime: time.Now().Add(-10 * time.Hour),
							ForwardedUrl:  "some-forwarded-url",
						}
						var err error
						signatureHeader, metadataHeader, err = route_service.BuildSignatureAndMetadata(crypto, signature)
						Expect(err).ToNot(HaveOccurred())
					})

					It("returns an route service request expired error", func() {
						err := config.ValidateSignature(headers)
						Expect(err).To(HaveOccurred())
						Expect(err).To(BeAssignableToTypeOf(route_service.RouteServiceExpired))
					})
				})
			})

			Context("when the header key does not match the previous key in the configuration", func() {
				BeforeEach(func() {
					var err error
					cryptoPrev, err = secure.NewAesGCM([]byte("QRSTUVWXYZ123456"))
					Expect(err).ToNot(HaveOccurred())
					config = route_service.NewRouteServiceConfig(true, 1*time.Hour, crypto, cryptoPrev)
				})

				It("rejects the signature", func() {
					err := config.ValidateSignature(headers)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("authentication failed"))
				})
			})
		})
	})
})
