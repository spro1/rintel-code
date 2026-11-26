package com.acme.order;

import com.acme.user.User;
import lombok.RequiredArgsConstructor;
import lombok.extern.slf4j.Slf4j;
import org.springframework.stereotype.Service;

@Slf4j
@Service
@RequiredArgsConstructor
public class OrderServiceImpl implements OrderService {
    private final OrderFacade facade;

    @Override
    public Order place(User user) {
        Money total = new Money(1000, "KRW");
        log.info("order for {}", user.getEmail());
        Order order = Order.builder().user(user).total(total).build();
        return order.getTotal().amount() > 0 ? order : null;
    }
}
