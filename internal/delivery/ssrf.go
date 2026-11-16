package delivery

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// lookupIPAddr — точка внедрения для тестов: позволяет подменить DNS.
var lookupIPAddr = net.DefaultResolver.LookupIPAddr

// forbiddenMetadataIPs — облачные metadata-сервисы.
// 169.254.169.254 уже покрыт link-local проверкой, оставлен явно.
var forbiddenMetadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"), // AWS/GCP/Azure metadata
	net.ParseIP("100.100.100.200"), // Alibaba Cloud metadata
}

// safeDialContext возвращает DialContext для http.Transport,
// который проверяет каждый IP до соединения и подключается
// напрямую к проверенному IP — без повторного DNS-резолва
// (защита от DNS rebinding).
func safeDialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid dial address %q: %w", addr, err)
		}

		ips, err := resolveAndValidate(ctx, host)
		if err != nil {
			return nil, err
		}

		// Пробуем проверенные адреса по порядку.
		var lastErr error
		for _, ip := range ips {
			conn, err := dialer.DialContext(ctx, network,
				net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

// resolveAndValidate возвращает список IP для host.
// IP-литерал проверяется напрямую; имя резолвится через DNS один раз.
// Если хотя бы один адрес запрещён — вся попытка отклоняется.
func resolveAndValidate(ctx context.Context, host string) ([]net.IP, error) {
	// Отбрасываем IPv6-зону (fe80::1%eth0) для проверки литерала.
	parseHost := host
	if i := strings.IndexByte(parseHost, '%'); i >= 0 {
		parseHost = parseHost[:i]
	}
	if ip := net.ParseIP(parseHost); ip != nil {
		if isForbiddenIP(ip) {
			return nil, fmt.Errorf("ssrf: forbidden IP address %s", ip)
		}
		return []net.IP{ip}, nil
	}

	addrs, err := lookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %q: no addresses", host)
	}

	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
		if isForbiddenIP(a.IP) {
			return nil, fmt.Errorf("ssrf: DNS name %q resolves to forbidden IP %s", host, a.IP)
		}
	}
	return ips, nil
}

// isForbiddenIP запрещает private, loopback, link-local, unspecified,
// multicast и metadata-адреса. IPv4-mapped IPv6 нормализуется к IPv4.
func isForbiddenIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
		return true
	}
	for _, meta := range forbiddenMetadataIPs {
		if ip.Equal(meta) {
			return true
		}
	}
	return false
}
