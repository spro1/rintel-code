package com.acme.order;

import com.acme.user.User;
import lombok.Builder;
import lombok.Getter;

@Getter
@Builder
public class Order {
    private final User user;
    private final Money total;
}
