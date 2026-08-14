package ingestion

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

func RedisRateLimit(client *redis.Client, limit int, window time.Duration) echo.MiddlewareFunc {
	if limit <= 0 {
		limit = 600
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			key := "agentray:rate:" + rateLimitIdentity(c)
			ctx := c.Request().Context()
			count, err := client.Incr(ctx, key).Result()
			if err != nil {
				return err
			}
			if count == 1 {
				if err := client.Expire(ctx, key, window).Err(); err != nil {
					return err
				}
			}
			c.Response().Header().Set("X-RateLimit-Limit", fmt.Sprint(limit))
			c.Response().Header().Set("X-RateLimit-Remaining", fmt.Sprint(maxInt(0, limit-int(count))))
			if count > int64(limit) {
				return echo.NewHTTPError(http.StatusTooManyRequests, "rate limit exceeded")
			}
			return next(c)
		}
	}
}

// AuthRateLimit throttles credential-verifying auth endpoints (login/signup) to
// blunt online password brute-force and account-enumeration. Unlike the ingest
// limiter it keys strictly on client IP (an attacker controls the body, not the
// source address) under a separate Redis namespace, and its default ceiling is
// deliberately low. Fail-open: a Redis error must not lock everyone out of login,
// so the request proceeds if the counter can't be read.
func AuthRateLimit(client *redis.Client, limit int, window time.Duration) echo.MiddlewareFunc {
	if limit <= 0 {
		limit = 20
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if client == nil {
				return next(c)
			}
			key := "agentray:authrate:" + rateLimitClientIP(c)
			ctx := c.Request().Context()
			count, err := client.Incr(ctx, key).Result()
			if err != nil {
				return next(c) // fail-open
			}
			if count == 1 {
				_ = client.Expire(ctx, key, window).Err()
			}
			if count > int64(limit) {
				return echo.NewHTTPError(http.StatusTooManyRequests, "too many attempts, try again later")
			}
			return next(c)
		}
	}
}

func rateLimitClientIP(c echo.Context) string {
	host := c.RealIP()
	if host == "" {
		host = strings.Split(c.Request().RemoteAddr, ":")[0]
	}
	return host
}

func rateLimitIdentity(c echo.Context) string {
	apiKey := c.Request().Header.Get("X-API-Key")
	if apiKey != "" {
		return apiKey
	}
	if token := c.QueryParam("token"); token != "" {
		return token
	}
	if key := c.QueryParam("api_key"); key != "" {
		return key
	}
	host := c.RealIP()
	if host == "" {
		host = strings.Split(c.Request().RemoteAddr, ":")[0]
	}
	return host
}

func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}
