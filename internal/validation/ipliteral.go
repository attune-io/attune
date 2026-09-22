/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package validation

import (
	"net"
	"strconv"
	"strings"
)

// alibabaIMDS is the Alibaba Cloud metadata address. It is not link-local,
// so the IP property checks do not catch it.
var alibabaIMDS = net.ParseIP("100.100.100.200").To4()

// hostIP returns a literal IP for hostname, including inet_aton forms that
// net.ParseIP rejects but OS resolvers still dial (short dotted forms,
// decimal, hex, and octal). A trailing DNS dot is ignored. A normal
// hostname returns nil.
func hostIP(hostname string) net.IP {
	host := strings.TrimSuffix(hostname, ".")
	if ip := net.ParseIP(host); ip != nil {
		return ip
	}
	return parseIPv4Literal(host)
}

func parseIPv4Literal(host string) net.IP {
	if host == "" || strings.Contains(host, ":") {
		return nil
	}
	parts := strings.Split(host, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return nil
	}
	nums := make([]uint64, len(parts))
	for i, part := range parts {
		n, ok := parseIPv4Part(part)
		if !ok {
			return nil
		}
		nums[i] = n
	}
	var acc uint64
	switch len(nums) {
	case 1:
		// inet_aton rejects a single value that does not fit in 32 bits.
		// Masking first would turn 2^32+127.0.0.1 into 127.0.0.1.
		if nums[0] > 0xffffffff {
			return nil
		}
		acc = nums[0]
	case 2:
		if nums[0] > 0xff || nums[1] > 0xffffff {
			return nil
		}
		acc = nums[0]<<24 | nums[1]
	case 3:
		if nums[0] > 0xff || nums[1] > 0xff || nums[2] > 0xffff {
			return nil
		}
		acc = nums[0]<<24 | nums[1]<<16 | nums[2]
	default:
		if nums[0] > 0xff || nums[1] > 0xff || nums[2] > 0xff || nums[3] > 0xff {
			return nil
		}
		acc = nums[0]<<24 | nums[1]<<16 | nums[2]<<8 | nums[3]
	}
	return net.IPv4(byte(acc>>24), byte(acc>>16), byte(acc>>8), byte(acc)).To4()
}

func parseIPv4Part(part string) (uint64, bool) {
	if part == "" {
		return 0, false
	}
	base := 10
	digits := part
	if len(part) > 1 && part[0] == '0' {
		if part[1] == 'x' || part[1] == 'X' {
			base = 16
			digits = part[2:]
			if digits == "" {
				return 0, false
			}
		} else {
			base = 8
		}
	}
	n, err := strconv.ParseUint(digits, base, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
